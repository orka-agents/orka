package store

import (
	"strings"

	"github.com/orka-agents/orka/internal/events"
)

const (
	ExecutionEventStreamTypeSession = events.ExecutionEventStreamTypeSession
)

// SanitizeExecutionEventPayloadFields applies the shared event redaction and
// truncation contract to store-facing event payload fields. The storage client
// applies it to every appended event before any provider call; a provider
// stores the payload it receives.
//
// It deliberately does NOT record redaction/truncation metrics: this function is
// invoked more than once per logical event (e.g. the harness mapper sanitizes,
// then the storage client sanitizes again on append), and a worker may have
// already redacted the payload in a different process before submission.
// Recording here therefore over- or under-counts. The storage client records
// the redaction/truncation metric exactly once per newly appended event, from
// the final event state (see ExecutionEventPayloadSanitizationSignals).
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

// ExecutionEventPayloadSanitizationSignals reports whether the final, persisted
// event payload shows evidence of redaction (the redaction marker is present in
// any text/JSON field) and/or truncation (truncation metadata is set). It is
// derived from the event's terminal state so it is correct regardless of which
// pass or process performed the sanitization, and is intended to be recorded
// exactly once per successful append.
func ExecutionEventPayloadSanitizationSignals(event *ExecutionEvent) (redacted, truncated bool) {
	if event == nil {
		return false, false
	}
	redacted = executionEventPayloadContainsRedactionMarker(event)
	truncated = event.Truncation != nil && !event.Truncation.Empty()
	return redacted, truncated
}

// MergeExecutionEventTruncation combines truncation metadata, preserving the
// highest original lengths without exposing raw values.
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
