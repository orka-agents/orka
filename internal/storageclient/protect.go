package storageclient

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

// The methods in this file apply Orka's redaction and sanitization policy
// before a provider sees the data, so a provider only ever stores content
// that Orka has already made safe to persist.

const unknownMetricLabel = "unknown"

// AppendExecutionEvent sanitizes the event payload before appending it.
func (c *Client) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	started := time.Now()
	prepared, err := c.prepareExecutionEvent(ctx, event)
	if err != nil {
		recordExecutionEventRejection(event, started)
		return nil, err
	}
	appended, err := c.StateProvider.AppendExecutionEvent(providerContext(ctx), prepared)
	if err != nil {
		return nil, err
	}
	recordExecutionEventSanitization(event, appended)
	return appended, nil
}

// AppendExecutionEventIfAbsent sanitizes the event payload before appending
// it under a deduplication key.
func (c *Client) AppendExecutionEventIfAbsent(
	ctx context.Context,
	event *store.ExecutionEvent,
	dedupeKey string,
) (*store.ExecutionEvent, bool, error) {
	started := time.Now()
	prepared, err := c.prepareExecutionEvent(ctx, event)
	if err != nil {
		recordExecutionEventRejection(event, started)
		return nil, false, err
	}
	appended, isNew, err := c.StateProvider.AppendExecutionEventIfAbsent(providerContext(ctx), prepared, dedupeKey)
	if err != nil {
		return nil, false, err
	}
	if isNew {
		recordExecutionEventSanitization(event, appended)
	}
	return appended, isNew, nil
}

// AppendExecutionEventWithPlanIfAbsent sanitizes the event payload before
// appending it together with its plan projection.
func (c *Client) AppendExecutionEventWithPlanIfAbsent(
	ctx context.Context,
	event *store.ExecutionEvent,
	dedupeKey string,
	plan *store.PlanState,
) (*store.ExecutionEvent, bool, error) {
	started := time.Now()
	prepared, err := c.prepareExecutionEvent(ctx, event)
	if err != nil {
		recordExecutionEventRejection(event, started)
		return nil, false, err
	}
	appended, isNew, err := c.StateProvider.AppendExecutionEventWithPlanIfAbsent(providerContext(ctx), prepared, dedupeKey, plan)
	if err != nil {
		return nil, false, err
	}
	if isNew {
		recordExecutionEventSanitization(event, appended)
	}
	return appended, isNew, nil
}

// AppendExecutionEventsIfAbsent sanitizes each event payload before the
// grouped append.
func (c *Client) AppendExecutionEventsIfAbsent(ctx context.Context, appends []store.ExecutionEventAppend) ([]store.ExecutionEventAppendResult, error) {
	started := time.Now()
	prepared := make([]store.ExecutionEventAppend, len(appends))
	for i, entry := range appends {
		event, err := c.prepareExecutionEvent(ctx, entry.Event)
		if err != nil {
			recordExecutionEventRejection(entry.Event, started)
			return nil, err
		}
		prepared[i] = store.ExecutionEventAppend{Event: event, DedupeKey: entry.DedupeKey}
	}
	results, err := c.StateProvider.AppendExecutionEventsIfAbsent(providerContext(ctx), prepared)
	if err != nil {
		return nil, err
	}
	for i, result := range results {
		if result.Appended {
			recordExecutionEventSanitization(appends[i].Event, result.Event)
		}
	}
	return results, nil
}

// prepareExecutionEvent returns a sanitized copy of event: payload fields are
// redacted and bounded, the truncation metadata records what was cut, and the
// usage observation the event carries is derived from the sanitized payload.
// Orka stamps the creation time when the caller left it unset, so a derived
// observation shares the event's time. The caller's event is not changed.
func (c *Client) prepareExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	if event == nil {
		return nil, store.ValidationErrorf("execution event is required")
	}
	prepared := cloneExecutionEvent(*event)
	prepared.Namespace = strings.TrimSpace(prepared.Namespace)
	prepared.StreamType = strings.TrimSpace(prepared.StreamType)
	if prepared.StreamType == "" {
		prepared.StreamType = store.ExecutionEventStreamTypeTask
	}
	prepared.StreamID = strings.TrimSpace(prepared.StreamID)
	prepared.Type = strings.TrimSpace(prepared.Type)
	prepared.TaskName = strings.TrimSpace(prepared.TaskName)
	prepared.SessionName = strings.TrimSpace(prepared.SessionName)
	if prepared.CreatedAt.IsZero() {
		prepared.CreatedAt = time.Now().UTC()
	} else {
		prepared.CreatedAt = prepared.CreatedAt.UTC()
	}
	if err := store.SanitizeExecutionEventPayloadFields(&prepared); err != nil {
		return nil, store.ValidationErrorf("invalid execution event payload: %v", err)
	}
	if err := deriveExecutionEventUsage(ctx, &prepared); err != nil {
		return nil, err
	}
	return &prepared, nil
}

// providerContext masks the controller's usage attribution once the client
// has derived usage from it, so a provider receives only the derived and
// redacted observation. Cancellation and transaction values are kept.
func providerContext(ctx context.Context) context.Context {
	if store.UsageAttributionFrom(ctx) == (store.UsageAttribution{}) {
		return ctx
	}
	return store.WithUsageAttribution(ctx, store.UsageAttribution{})
}

func cloneExecutionEvent(event store.ExecutionEvent) store.ExecutionEvent {
	if event.Usage != nil {
		usage := *event.Usage
		event.Usage = &usage
	}
	if event.Content != nil {
		event.Content = append(json.RawMessage(nil), event.Content...)
	}
	if event.Truncation != nil {
		truncation := *event.Truncation
		event.Truncation = &truncation
	}
	return event
}

// recordExecutionEventRejection counts an append the client refused before
// calling the provider, such as a payload that cannot be sanitized or an
// invalid worker usage report, in the same append failure and duration
// metrics the provider records for its own failures.
func recordExecutionEventRejection(event *store.ExecutionEvent, started time.Time) {
	streamType, eventType := executionEventMetricLabels(event)
	metrics.RecordExecutionEventAppend(streamType, eventType, false, time.Since(started).Seconds())
}

// recordExecutionEventSanitization counts redaction and truncation once per
// newly persisted event, from its final stored state.
func recordExecutionEventSanitization(submitted, appended *store.ExecutionEvent) {
	streamType, eventType := executionEventMetricLabels(submitted)
	redacted, truncated := store.ExecutionEventPayloadSanitizationSignals(appended)
	metrics.RecordExecutionEventPayloadSanitization(streamType, eventType, redacted, truncated)
}

func executionEventMetricLabels(event *store.ExecutionEvent) (string, string) {
	if event == nil {
		return unknownMetricLabel, unknownMetricLabel
	}
	streamType := strings.TrimSpace(event.StreamType)
	if streamType == "" {
		streamType = store.ExecutionEventStreamTypeTask
	}
	if !events.IsValidExecutionEventStreamType(streamType) {
		streamType = unknownMetricLabel
	}
	eventType := strings.TrimSpace(event.Type)
	if !events.IsValidExecutionEventType(eventType) {
		eventType = unknownMetricLabel
	}
	return streamType, eventType
}

// CreateMemory redacts the memory content before storing it. The caller's
// record receives the redacted content, as the stored copy does.
func (c *Client) CreateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return store.ValidationErrorf("memory is required")
	}
	memory.Content = redact.SensitiveText(memory.Content)
	return c.StateProvider.CreateMemory(ctx, memory)
}

// UpdateMemory redacts the memory content before storing it.
func (c *Client) UpdateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return store.ValidationErrorf("memory is required")
	}
	memory.Content = redact.SensitiveText(memory.Content)
	return c.StateProvider.UpdateMemory(ctx, memory)
}

// CreateMemoryProposal redacts every free-text proposal field before storing
// the proposal.
func (c *Client) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	if proposal == nil {
		return store.ValidationErrorf("proposal is required")
	}
	proposal.Title = redact.SensitiveText(proposal.Title)
	proposal.Description = redact.SensitiveText(proposal.Description)
	proposal.Content = redact.SensitiveText(proposal.Content)
	proposal.Patch = redact.SensitiveText(proposal.Patch)
	proposal.Reviewer = redact.SensitiveText(proposal.Reviewer)
	proposal.ReviewNote = redact.SensitiveText(proposal.ReviewNote)
	proposal.AppliedBy = redact.SensitiveText(proposal.AppliedBy)
	return c.StateProvider.CreateMemoryProposal(ctx, proposal)
}

// ReviewMemoryProposal redacts the reviewer and review note.
func (c *Client) ReviewMemoryProposal(ctx context.Context, review store.MemoryProposalReview) error {
	review.Reviewer = redact.SensitiveText(review.Reviewer)
	review.ReviewNote = redact.SensitiveText(review.ReviewNote)
	return c.StateProvider.ReviewMemoryProposal(ctx, review)
}

// SaveThreatModel sanitizes the threat model content before storing it. The
// caller's record receives the sanitized content.
func (c *Client) SaveThreatModel(ctx context.Context, model *store.ThreatModel) error {
	return saveThreatModel(ctx, c.StateProvider, model)
}

// SaveThreatModelIfVersion sanitizes the threat model content before the
// conditional save.
func (c *Client) SaveThreatModelIfVersion(ctx context.Context, model *store.ThreatModel, expectedVersion int64) error {
	return saveThreatModelIfVersion(ctx, c.StateProvider, model, expectedVersion)
}

type threatModelWriter interface {
	SaveThreatModel(ctx context.Context, model *store.ThreatModel) error
	SaveThreatModelIfVersion(ctx context.Context, model *store.ThreatModel, expectedVersion int64) error
}

func saveThreatModel(ctx context.Context, writer threatModelWriter, model *store.ThreatModel) error {
	if model == nil {
		return store.ValidationErrorf("threat model is required")
	}
	model.Content = security.SanitizeThreatModel(model.Content)
	return writer.SaveThreatModel(ctx, model)
}

func saveThreatModelIfVersion(ctx context.Context, writer threatModelWriter, model *store.ThreatModel, expectedVersion int64) error {
	if model == nil {
		return store.ValidationErrorf("threat model is required")
	}
	model.Content = security.SanitizeThreatModel(model.Content)
	return writer.SaveThreatModelIfVersion(ctx, model, expectedVersion)
}

// RecordUsage redacts and bounds the provider and model names before storing
// the observation.
func (c *Client) RecordUsage(ctx context.Context, observation store.UsageObservation) error {
	return c.StateProvider.RecordUsage(ctx, protectUsageObservation(observation))
}

func protectUsageObservation(observation store.UsageObservation) store.UsageObservation {
	observation.Provider, _, _ = events.RedactAndTruncateExecutionEventText(observation.Provider, maxUsageNameChars)
	observation.Model, _, _ = events.RedactAndTruncateExecutionEventText(observation.Model, maxUsageNameChars)
	return observation
}

// maxUsageNameChars bounds stored provider and model names.
const maxUsageNameChars = 256
