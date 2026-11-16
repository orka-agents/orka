package storageclient

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

// The methods in this file apply Orka's redaction and sanitization policy
// before a provider sees the data, so a provider only ever stores content
// that Orka has already made safe to persist. A request whose caller supplied
// its own write envelope is sent unchanged (see identity.go), so for it they
// only check the policy: a request the policy would change is refused with
// ErrValidation, and one it would not change is sent as it is.

// protectText redacts value, or returns a caller-owned value as it is and
// refuses one redaction would change.
func protectText(ctx context.Context, method, field, value string) (string, error) {
	redacted := redact.SensitiveText(value)
	if redacted != value && callerOwnsEnvelope(ctx) {
		return "", unprotected(method, field)
	}
	return redacted, nil
}

// protectTexts redacts every field in place, or, for a caller-owned request,
// leaves them all as they are and refuses the request if redaction would
// change any. Nothing is changed when it refuses.
func protectTexts(ctx context.Context, method string, fields map[string]*string) error {
	redacted := make(map[string]string, len(fields))
	for name, field := range fields {
		value, err := protectText(ctx, method, name, *field)
		if err != nil {
			return err
		}
		redacted[name] = value
	}
	for name, field := range fields {
		*field = redacted[name]
	}
	return nil
}

// AppendExecutionEvent sanitizes the event payload before appending it.
func (c *Client) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	started := time.Now()
	prepared, err := c.prepareExecutionEvent(ctx, "AppendExecutionEvent", event)
	if err != nil {
		recordExecutionEventAppend(event, false, started)
		return nil, err
	}
	appended, err := c.StateProvider.AppendExecutionEvent(providerContext(ctx), prepared)
	recordExecutionEventAppend(event, err == nil, started)
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
	prepared, err := c.prepareExecutionEvent(ctx, "AppendExecutionEventIfAbsent", event)
	if err != nil {
		recordExecutionEventAppend(event, false, started)
		return nil, false, err
	}
	if _, err := store.NormalizeExecutionEventDedupeKey(dedupeKey); err != nil {
		return nil, false, err
	}
	appended, isNew, err := c.StateProvider.AppendExecutionEventIfAbsent(providerContext(ctx), prepared, dedupeKey)
	recordExecutionEventAppend(event, err == nil, started)
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
	prepared, err := c.prepareExecutionEvent(ctx, "AppendExecutionEventWithPlanIfAbsent", event)
	if err != nil {
		recordExecutionEventAppend(event, false, started)
		return nil, false, err
	}
	if _, err := store.NormalizeExecutionEventDedupeKey(dedupeKey); err != nil {
		return nil, false, err
	}
	if plan == nil {
		return nil, false, store.ValidationErrorf("plan state is required")
	}
	appended, isNew, err := c.StateProvider.AppendExecutionEventWithPlanIfAbsent(providerContext(ctx), prepared, dedupeKey, plan)
	recordExecutionEventAppend(event, err == nil, started)
	if err != nil {
		return nil, false, err
	}
	if isNew {
		recordExecutionEventSanitization(event, appended)
	}
	return appended, isNew, nil
}

// AppendExecutionEventsIfAbsent sanitizes each event payload before the
// grouped append. Each event in the group counts as one append attempt: a
// refused group counts every event as failed, as does a provider error, since
// the provider commits a group in one transaction. A deduplicated replay
// counts as appended, and an invalid deduplication key is refused before the
// provider is called and is not counted, as for the single-event methods.
func (c *Client) AppendExecutionEventsIfAbsent(ctx context.Context, appends []store.ExecutionEventAppend) ([]store.ExecutionEventAppendResult, error) {
	started := time.Now()
	prepared := make([]store.ExecutionEventAppend, len(appends))
	for i, entry := range appends {
		event, err := c.prepareExecutionEvent(ctx, "AppendExecutionEventsIfAbsent", entry.Event)
		if err != nil {
			recordExecutionEventGroupAppend(appends, false, started)
			return nil, err
		}
		prepared[i] = store.ExecutionEventAppend{Event: event, DedupeKey: entry.DedupeKey}
	}
	for _, entry := range appends {
		if _, err := store.NormalizeExecutionEventDedupeKey(entry.DedupeKey); err != nil {
			return nil, err
		}
	}
	results, err := c.StateProvider.AppendExecutionEventsIfAbsent(providerContext(ctx), prepared)
	recordExecutionEventGroupAppend(appends, err == nil, started)
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
// A caller-owned event that preparation would change is refused.
func (c *Client) prepareExecutionEvent(ctx context.Context, method string, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
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
	if callerOwnsEnvelope(ctx) && !sameExecutionEvent(prepared, *event) {
		return nil, unprotected(method, "the execution event")
	}
	return &prepared, nil
}

// sameExecutionEvent reports whether preparation left an event as it was.
// Content compares as bytes and the creation time as an instant.
func sameExecutionEvent(prepared, original store.ExecutionEvent) bool {
	if !bytes.Equal(prepared.Content, original.Content) || !prepared.CreatedAt.Equal(original.CreatedAt) {
		return false
	}
	prepared.Content, original.Content = nil, nil
	prepared.CreatedAt, original.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(prepared, original)
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

// CreateMemory redacts the memory content before storing it, and gives a
// memory without an ID or times one. The caller's record receives the
// redacted content and the assigned identity, as the stored copy does.
func (c *Client) CreateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return store.ValidationErrorf("memory is required")
	}
	content, err := protectText(ctx, "CreateMemory", "the memory's content", memory.Content)
	if err != nil {
		return err
	}
	memory.Content = content
	if callerOwnsEnvelope(ctx) {
		if err := requireStampedMemory(memory); err != nil {
			return err
		}
		return c.StateProvider.CreateMemory(ctx, memory)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	identifyMemory(memory, now)
	return c.StateProvider.CreateMemory(ctx, memory)
}

// UpdateMemory redacts the memory content before storing it.
func (c *Client) UpdateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return store.ValidationErrorf("memory is required")
	}
	content, err := protectText(ctx, "UpdateMemory", "the memory's content", memory.Content)
	if err != nil {
		return err
	}
	memory.Content = content
	return c.StateProvider.UpdateMemory(ctx, memory)
}

// CreateMemoryProposal redacts every free-text proposal field before storing
// the proposal, and gives a proposal without an ID or times one.
func (c *Client) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	if proposal == nil {
		return store.ValidationErrorf("proposal is required")
	}
	if err := protectTexts(ctx, "CreateMemoryProposal", map[string]*string{
		"the proposal's title":       &proposal.Title,
		"the proposal's description": &proposal.Description,
		"the proposal's content":     &proposal.Content,
		"the proposal's patch":       &proposal.Patch,
		"the proposal's reviewer":    &proposal.Reviewer,
		"the proposal's review note": &proposal.ReviewNote,
		"the proposal's applier":     &proposal.AppliedBy,
	}); err != nil {
		return err
	}
	if callerOwnsEnvelope(ctx) {
		if err := requireStampedMemoryProposal(proposal); err != nil {
			return err
		}
		return c.StateProvider.CreateMemoryProposal(ctx, proposal)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	identifyMemoryProposal(proposal, now)
	return c.StateProvider.CreateMemoryProposal(ctx, proposal)
}

// ReviewMemoryProposal redacts the reviewer and review note.
func (c *Client) ReviewMemoryProposal(ctx context.Context, review store.MemoryProposalReview) error {
	if err := protectTexts(ctx, "ReviewMemoryProposal", map[string]*string{
		"the reviewer":    &review.Reviewer,
		"the review note": &review.ReviewNote,
	}); err != nil {
		return err
	}
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
	if err := protectThreatModel(ctx, "SaveThreatModel", model); err != nil {
		return err
	}
	return writer.SaveThreatModel(ctx, model)
}

func saveThreatModelIfVersion(ctx context.Context, writer threatModelWriter, model *store.ThreatModel, expectedVersion int64) error {
	if model == nil {
		return store.ValidationErrorf("threat model is required")
	}
	if err := protectThreatModel(ctx, "SaveThreatModelIfVersion", model); err != nil {
		return err
	}
	return writer.SaveThreatModelIfVersion(ctx, model, expectedVersion)
}

// protectThreatModel sanitizes the model's content in place, or leaves a
// caller-owned model as it is and refuses one sanitizing would change.
func protectThreatModel(ctx context.Context, method string, model *store.ThreatModel) error {
	sanitized := security.SanitizeThreatModel(model.Content)
	if sanitized != model.Content && callerOwnsEnvelope(ctx) {
		return unprotected(method, "the threat model's content")
	}
	model.Content = sanitized
	return nil
}

// RecordUsage redacts and bounds the provider and model names before storing
// the observation.
func (c *Client) RecordUsage(ctx context.Context, observation store.UsageObservation) error {
	protected := protectUsageObservation(observation)
	if callerOwnsEnvelope(ctx) {
		if protected.Provider != observation.Provider || protected.Model != observation.Model {
			return unprotected("RecordUsage", "the provider or model name")
		}
		protected = observation
	}
	return c.StateProvider.RecordUsage(ctx, protected)
}

func protectUsageObservation(observation store.UsageObservation) store.UsageObservation {
	observation.Provider, _, _ = events.RedactAndTruncateExecutionEventText(observation.Provider, maxUsageNameChars)
	observation.Model, _, _ = events.RedactAndTruncateExecutionEventText(observation.Model, maxUsageNameChars)
	return observation
}

// maxUsageNameChars bounds stored provider and model names.
const maxUsageNameChars = 256
