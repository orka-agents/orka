package eventjournal

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	maxOpenToolAccumulators     = 128
	maxBufferedToolContentBytes = 1 << 20
)

// Journal owns duplicate-safe persistence of mapped harness v2 updates.
type Journal struct {
	EventStore       store.ExecutionEventStore
	MapContext       MapContext
	RecoveryIdentity MappedUpdateIdentity
}

// State is the mutable aggregation state for one non-reconnectable prompt
// stream. Public lifecycle/telemetry updates are durable as they arrive.
// Untrusted assistant/tool text becomes durable only after its logical stream
// ends, so redaction can see credential shapes split across protocol frames.
// Controller takeover deliberately terminalizes accepted prompts as
// OutcomeUnknown rather than replaying or persisting a raw-content crash buffer.
type State struct {
	journal                     Journal
	promptIdentity              MappedUpdateIdentity
	processedSequence           uint64
	aggregatedSequence          uint64
	assistantTranscriptSequence uint64
	terminalUsageSequence       uint64
	promptAcceptedSequence      uint64
	promptTerminalSequence      uint64
	toolClosureSequences        map[uint64]struct{}
	toolText                    map[string]*streamText
	overflowToolIDs             map[string]struct{}
	untrackedOverflowTool       bool
	toolBufferedBytes           int
}

type streamText struct {
	text                  strings.Builder
	runes                 int
	overflow              bool
	multipleBlocksOmitted bool
	title                 string
	kind                  string
	lastEvent             harnessv2.Event
	hasEvent              bool
}

func (s *streamText) append(value string) {
	if s == nil || s.overflow || s.multipleBlocksOmitted || value == "" {
		return
	}
	runes := utf8.RuneCountInString(value)
	remaining := executionevents.MaxExecutionEventContentTextChars - s.runes
	if runes > remaining {
		// A truncated prefix cannot be redacted safely when a credential spans
		// the cutoff. Discard all buffered text and persist only truncation
		// metadata when the tool reaches a terminal update.
		s.omitForOverflow()
		return
	}
	s.text.WriteString(value)
	s.runes += runes
}

func (s *streamText) replace(value string) {
	if s == nil {
		return
	}
	s.text.Reset()
	s.runes = 0
	s.overflow = false
	s.multipleBlocksOmitted = false
	s.append(value)
}

func (s *streamText) omitMultipleBlocks() {
	if s == nil {
		return
	}
	s.text.Reset()
	s.runes = 0
	s.overflow = false
	s.multipleBlocksOmitted = true
}

func (s *streamText) omitForOverflow() {
	if s == nil {
		return
	}
	s.text.Reset()
	s.runes = 0
	s.overflow = true
	s.multipleBlocksOmitted = false
}

// HasUpdate reports whether event was already persisted or processed during
// this journal pass. Validated harness streams use strictly increasing event
// sequences, so one high-water mark bounds deduplication state for any prompt
// duration.
func (s *State) HasUpdate(event harnessv2.Event) bool {
	if s == nil {
		return false
	}
	identity := mappedUpdateIdentity(event)
	return identity.valid() && s.promptIdentity.promptValid() &&
		identity.samePrompt(s.promptIdentity) && identity.Sequence <= s.processedSequence
}

// Open loads persisted harness v2 sequence state for the current prompt.
func (j Journal) Open(ctx context.Context) (*State, error) {
	if j.EventStore == nil {
		return nil, fmt.Errorf("execution event store is required")
	}
	if err := j.MapContext.validate(); err != nil {
		return nil, err
	}
	mapCtx := j.MapContext.normalized()
	state := &State{
		journal:              j,
		promptIdentity:       j.RecoveryIdentity,
		toolClosureSequences: map[uint64]struct{}{},
		toolText:             map[string]*streamText{},
		overflowToolIDs:      map[string]struct{}{},
	}
	var err error
	if j.RecoveryIdentity.promptValid() {
		err = j.loadCurrentPromptIdentities(ctx, mapCtx, state)
	} else {
		err = j.loadAllIdentities(ctx, mapCtx, state)
	}
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (j Journal) loadAllIdentities(
	ctx context.Context,
	mapCtx MapContext,
	state *State,
) error {
	var afterSeq int64
	for {
		listed, err := j.EventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  mapCtx.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   mapCtx.StreamID,
			AfterSeq:   afterSeq,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return fmt.Errorf("list mapped harness v2 updates: %w", err)
		}
		if len(listed) == 0 {
			break
		}
		for _, event := range listed {
			if event.Seq > afterSeq {
				afterSeq = event.Seq
			}
			if identity, _, kind, ok := mappedExecutionEventRecord(event); ok {
				if !state.promptIdentity.promptValid() || !identity.samePrompt(state.promptIdentity) {
					state.resetPrompt(identity)
				}
				state.observePersisted(identity, kind)
			}
		}
		if len(listed) < store.MaxExecutionEventLimit {
			break
		}
	}
	return nil
}

func (j Journal) loadCurrentPromptIdentities(
	ctx context.Context,
	mapCtx MapContext,
	state *State,
) error {
	pageEnd, err := j.EventStore.GetLatestExecutionEventSeq(
		ctx, mapCtx.Namespace, store.ExecutionEventStreamTypeTask, mapCtx.StreamID,
	)
	if err != nil {
		return fmt.Errorf("get latest mapped harness v2 update sequence: %w", err)
	}
	// Per-stream sequence numbers are monotonic and gap-tolerant. Fixed-width
	// AfterSeq windows let recovery walk backward without expanding the store API.
	for pageEnd > 0 {
		pageStart := max(int64(0), pageEnd-int64(store.MaxExecutionEventLimit))
		listed, err := j.EventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  mapCtx.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   mapCtx.StreamID,
			AfterSeq:   pageStart,
			Limit:      int(pageEnd - pageStart),
		})
		if err != nil {
			return fmt.Errorf("list current-prompt mapped harness v2 updates: %w", err)
		}
		for index := len(listed) - 1; index >= 0; index-- {
			event := listed[index]
			if event.Seq > pageEnd {
				continue
			}
			identity, _, kind, ok := mappedExecutionEventRecord(event)
			if !ok {
				continue
			}
			if !identity.samePrompt(j.RecoveryIdentity) {
				return nil
			}
			state.observePersisted(identity, kind)
		}
		pageEnd = pageStart
	}
	return nil
}

func (s *State) resetPrompt(identity MappedUpdateIdentity) {
	s.promptIdentity = identity
	s.processedSequence = 0
	s.aggregatedSequence = 0
	s.assistantTranscriptSequence = 0
	s.terminalUsageSequence = 0
	s.promptAcceptedSequence = 0
	s.promptTerminalSequence = 0
	clear(s.toolClosureSequences)
	clear(s.overflowToolIDs)
	s.untrackedOverflowTool = false
}

func (s *State) bindPrompt(identity MappedUpdateIdentity) error {
	if !identity.valid() {
		return fmt.Errorf("valid harness v2 update identity is required")
	}
	if !s.promptIdentity.promptValid() {
		s.promptIdentity = identity
		return nil
	}
	if !identity.samePrompt(s.promptIdentity) {
		return fmt.Errorf("harness v2 update prompt identity does not match journal state")
	}
	return nil
}

func (s *State) observePersisted(identity MappedUpdateIdentity, kind mappedJournalRecordKind) {
	if identity.Sequence > s.processedSequence {
		s.processedSequence = identity.Sequence
	}
	if identity.Sequence > s.aggregatedSequence {
		s.aggregatedSequence = identity.Sequence
	}
	switch kind {
	case mappedJournalRecordAssistantTranscript:
		if identity.Sequence > s.assistantTranscriptSequence {
			s.assistantTranscriptSequence = identity.Sequence
		}
	case mappedJournalRecordToolStreamClosure:
		s.rememberToolClosure(identity.Sequence)
	case mappedJournalRecordTerminalUsage:
		if identity.Sequence > s.terminalUsageSequence {
			s.terminalUsageSequence = identity.Sequence
		}
	case mappedJournalRecordPromptAccepted:
		if identity.Sequence > s.promptAcceptedSequence {
			s.promptAcceptedSequence = identity.Sequence
		}
	case mappedJournalRecordPromptTerminal:
		if identity.Sequence > s.promptTerminalSequence {
			s.promptTerminalSequence = identity.Sequence
		}
	}
}

func (s *State) markProcessed(identity MappedUpdateIdentity) {
	if identity.Sequence > s.processedSequence {
		s.processedSequence = identity.Sequence
	}
}

func (s *State) rememberToolClosure(sequence uint64) {
	if _, ok := s.toolClosureSequences[sequence]; ok || len(s.toolClosureSequences) >= maxOpenToolAccumulators {
		return
	}
	s.toolClosureSequences[sequence] = struct{}{}
}

// AppendUpdateIfNew maps and appends event unless its full protocol identity is
// already persisted or was processed earlier in this pass. Assistant chunks
// and content-only tool fragments are retained only for same-pass
// deduplication; complete assistant/tool text is persisted only after the
// logical stream ends so redaction sees update boundaries.
func (s *State) AppendUpdateIfNew(ctx context.Context, event harnessv2.Event) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	identity := mappedUpdateIdentity(event)
	mapped, err := MapUpdate(event, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	if err := s.bindPrompt(identity); err != nil {
		return nil, false, err
	}
	if s.HasUpdate(event) {
		s.finishToolUpdate(event)
		return nil, false, nil
	}
	if s.aggregateToolUpdate(event, identity.Sequence) {
		// The journal's in-memory telemetry bound must not abort a valid
		// prompt. Omit excess open-tool updates until capacity is available.
		s.markProcessed(identity)
		return nil, false, nil
	}
	if event.Update.Kind == harnessv2.UpdateAssistantMessageChunk {
		// The complete terminal transcript is persisted once redaction can see
		// all chunk boundaries. Advance only the bounded sequence high-water.
		s.markProcessed(identity)
		return nil, false, nil
	}
	if isBufferedToolContentUpdate(event) {
		// Keep streamed output in the accumulator, but do not emit an unbounded
		// series of ToolCallStarted events. The terminal tool event or prompt
		// stream closure receives the complete, redacted text.
		s.markProcessed(identity)
		return nil, false, nil
	}
	if contentText, truncated, multipleBlocksOmitted, title, kind, ok := s.finishToolUpdate(event); ok {
		mappedEvent := withBufferedToolMetadata(event, title, kind)
		mapped, err = mapToolUpdateWithContent(
			mappedEvent, s.journal.MapContext, contentText, truncated, multipleBlocksOmitted,
		)
		if err != nil {
			return nil, false, err
		}
	} else if contentText, truncated, multipleBlocksOmitted, title, kind, ok := transientTerminalToolUpdate(event); ok {
		mappedEvent := withBufferedToolMetadata(event, title, kind)
		mapped, err = mapToolUpdateWithContent(
			mappedEvent, s.journal.MapContext, contentText, truncated, multipleBlocksOmitted,
		)
		if err != nil {
			return nil, false, err
		}
	} else if isNonTerminalToolUpdate(event) {
		mapped, err = mapToolUpdateWithoutMetadata(event, s.journal.MapContext)
		if err != nil {
			return nil, false, err
		}
	}
	return s.appendMappedEvent(
		ctx, identity, mappedJournalRecordUpdate, mapped, "append mapped harness v2 update",
	)
}

func isBufferedToolContentUpdate(event harnessv2.Event) bool {
	if event.Update == nil || event.Update.Kind != harnessv2.UpdateToolCallUpdate || event.Update.ToolCall == nil {
		return false
	}
	tool := event.Update.ToolCall
	content, multipleBlocks := toolContentFragment(tool.Content)
	return (tool.ContentOmitted || tool.ContentReplace || content != "" || multipleBlocks) &&
		tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed
}

func isNonTerminalToolUpdate(event harnessv2.Event) bool {
	if event.Update == nil || event.Update.ToolCall == nil {
		return false
	}
	status := event.Update.ToolCall.Status
	return status != harnessv2.ToolCallStatusCompleted && status != harnessv2.ToolCallStatusFailed
}

// AppendAssistantTranscriptIfNew persists a complete terminal transcript as
// one model-message event. Redaction therefore sees secrets split across any
// number of streamed assistant chunks before durable storage.
func (s *State) AppendAssistantTranscriptIfNew(
	ctx context.Context,
	terminal harnessv2.Event,
	transcript string,
	contentOmitted bool,
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	if transcript == "" && !contentOmitted {
		return nil, false, nil
	}
	identity := mappedUpdateIdentity(terminal)
	if err := s.bindPrompt(identity); err != nil {
		return nil, false, err
	}
	if s.assistantTranscriptSequence == identity.Sequence {
		return nil, false, nil
	}
	mapped, err := MapAssistantTranscript(terminal, s.journal.MapContext, transcript, contentOmitted)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(
		ctx, identity, mappedJournalRecordAssistantTranscript, mapped,
		"append mapped harness v2 assistant transcript",
	)
}

// AppendTerminalUsageIfNew persists usage carried only by a completed result.
// It uses a distinct journal identity so the same terminal event can also own
// the final assistant transcript without ambiguous retry reconciliation.
func (s *State) AppendTerminalUsageIfNew(
	ctx context.Context,
	terminal harnessv2.Event,
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	if terminal.Type != harnessv2.EventCompleted || terminal.Completed == nil {
		return nil, false, fmt.Errorf("completed harness v2 event is required")
	}
	if !hasUsageTelemetry(terminal.Completed.Result.Usage) {
		return nil, false, nil
	}
	identity := mappedUpdateIdentity(terminal)
	if err := s.bindPrompt(identity); err != nil {
		return nil, false, err
	}
	if s.terminalUsageSequence == identity.Sequence {
		return nil, false, nil
	}
	mapped, err := MapTerminalUsage(terminal, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(
		ctx, identity, mappedJournalRecordTerminalUsage, mapped,
		"append mapped harness v2 terminal usage",
	)
}

// AppendPromptLifecycleIfNew persists accepted and terminal model-request
// lifecycle events with distinct identities from terminal usage/transcripts.
func (s *State) AppendPromptLifecycleIfNew(
	ctx context.Context,
	event harnessv2.Event,
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	identity := mappedUpdateIdentity(event)
	if err := s.bindPrompt(identity); err != nil {
		return nil, false, err
	}
	var kind mappedJournalRecordKind
	switch {
	case event.Type == harnessv2.EventAccepted:
		if s.promptAcceptedSequence == identity.Sequence {
			return nil, false, nil
		}
		kind = mappedJournalRecordPromptAccepted
	case event.Type.IsTerminal():
		if s.promptTerminalSequence == identity.Sequence {
			return nil, false, nil
		}
		kind = mappedJournalRecordPromptTerminal
	default:
		return nil, false, fmt.Errorf("accepted or terminal harness v2 event is required")
	}
	mapped, err := MapPromptLifecycle(event, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(ctx, identity, kind, mapped, "append mapped harness v2 prompt lifecycle")
}

// AppendAssistantStreamClosureIfNew persists the complete assistant text seen
// before a non-terminal stream closure. The last assistant update supplies the
// durable protocol identity; the complete buffered text supplies the redaction
// boundary that individual chunks cannot provide safely.
func (s *State) AppendAssistantStreamClosureIfNew(
	ctx context.Context,
	lastUpdate harnessv2.Event,
	transcript string,
	contentOmitted bool,
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	if transcript == "" && !contentOmitted {
		return nil, false, nil
	}
	if lastUpdate.Type != harnessv2.EventUpdate || lastUpdate.Update == nil || lastUpdate.Update.AssistantMessage == nil {
		return nil, false, fmt.Errorf("last assistant update is required")
	}
	identity := mappedUpdateIdentity(lastUpdate)
	if err := s.bindPrompt(identity); err != nil {
		return nil, false, err
	}
	if s.assistantTranscriptSequence == identity.Sequence {
		return nil, false, nil
	}
	mapped, err := MapAssistantTranscript(lastUpdate, s.journal.MapContext, transcript, contentOmitted)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(
		ctx, identity, mappedJournalRecordAssistantTranscript, mapped,
		"append mapped harness v2 assistant stream closure",
	)
}

// AppendBufferedStreamsIfNew persists buffered assistant and interrupted tool
// streams in the order of their last protocol updates. assistantEvent supplies
// the durable identity; assistantOrderSequence is the last assistant chunk's
// sequence, or the assistant event sequence when no chunks preceded it. An
// omitted assistant stream persists truncation metadata without unsafe text.
func (s *State) AppendBufferedStreamsIfNew(
	ctx context.Context,
	assistantEvent *harnessv2.Event,
	assistantOrderSequence uint64,
	transcript string,
	assistantContentOmitted bool,
) error {
	if s == nil {
		return fmt.Errorf("harness v2 journal state is required")
	}
	type pendingStreamClosure struct {
		sequence    uint64
		toolID      string
		accumulator *streamText
		assistant   bool
	}
	closures := make([]pendingStreamClosure, 0, len(s.toolText)+1)
	for toolID, accumulator := range s.toolText {
		if accumulator == nil || !accumulator.hasEvent {
			continue
		}
		closures = append(closures, pendingStreamClosure{
			sequence: accumulator.lastEvent.Identity.Sequence, toolID: toolID, accumulator: accumulator,
		})
	}
	if transcript != "" || assistantContentOmitted {
		if assistantEvent == nil {
			return fmt.Errorf("assistant event is required for a buffered transcript")
		}
		identity := mappedUpdateIdentity(*assistantEvent)
		if assistantOrderSequence == 0 || assistantOrderSequence > identity.Sequence {
			return fmt.Errorf("assistant order sequence must be in range 1..%d", identity.Sequence)
		}
		closures = append(closures, pendingStreamClosure{sequence: assistantOrderSequence, assistant: true})
	}
	sort.Slice(closures, func(i, j int) bool {
		if closures[i].sequence == closures[j].sequence {
			if closures[i].assistant != closures[j].assistant {
				return closures[i].assistant
			}
			return closures[i].toolID < closures[j].toolID
		}
		return closures[i].sequence < closures[j].sequence
	})
	for _, closure := range closures {
		if closure.assistant {
			var err error
			if assistantEvent.Type.IsTerminal() {
				_, _, err = s.AppendAssistantTranscriptIfNew(ctx, *assistantEvent, transcript, assistantContentOmitted)
			} else {
				_, _, err = s.AppendAssistantStreamClosureIfNew(ctx, *assistantEvent, transcript, assistantContentOmitted)
			}
			if err != nil {
				return err
			}
			continue
		}
		if err := s.appendToolStreamClosureIfNew(ctx, closure.toolID, closure.accumulator); err != nil {
			return err
		}
	}
	return nil
}

// AppendToolStreamClosuresIfNew persists buffered output for tools that did
// not emit a completed/failed update before the prompt stream closed. The last
// observed tool update supplies the durable protocol identity.
func (s *State) AppendToolStreamClosuresIfNew(ctx context.Context) error {
	return s.AppendBufferedStreamsIfNew(ctx, nil, 0, "", false)
}

func (s *State) appendToolStreamClosureIfNew(ctx context.Context, toolID string, accumulator *streamText) error {
	identity := mappedUpdateIdentity(accumulator.lastEvent)
	if err := s.bindPrompt(identity); err != nil {
		return err
	}
	if _, ok := s.toolClosureSequences[identity.Sequence]; ok {
		s.removeToolAccumulator(toolID)
		return nil
	}
	mappedEvent := withInterruptedToolClosure(accumulator.lastEvent, accumulator.title, accumulator.kind)
	mapped, err := mapToolStreamClosure(
		mappedEvent, s.journal.MapContext, accumulator.text.String(), accumulator.overflow,
		accumulator.multipleBlocksOmitted,
	)
	if err != nil {
		return err
	}
	if _, _, err := s.appendMappedEvent(
		ctx, identity, mappedJournalRecordToolStreamClosure, mapped,
		"append mapped harness v2 tool stream closure",
	); err != nil {
		return err
	}
	s.removeToolAccumulator(toolID)
	return nil
}

// appendMappedEvent uses the store's stream-scoped atomic deduplication key,
// then reconciles an ambiguous append error against the durable stream before
// one safe retry.
func (s *State) appendMappedEvent(
	ctx context.Context,
	identity MappedUpdateIdentity,
	kind mappedJournalRecordKind,
	mapped *store.ExecutionEvent,
	operation string,
) (*store.ExecutionEvent, bool, error) {
	key := identity.Key()
	switch kind {
	case mappedJournalRecordToolStreamClosure:
		key = mappedToolStreamClosureKey(identity)
	case mappedJournalRecordTerminalUsage:
		key = mappedTerminalUsageKey(identity)
	case mappedJournalRecordPromptAccepted:
		key = mappedPromptLifecycleKey(identity, mappedPromptAcceptedKind)
	case mappedJournalRecordPromptTerminal:
		key = mappedPromptLifecycleKey(identity, mappedPromptTerminalKind)
	}
	deduplicatingStore, ok := s.journal.EventStore.(store.DeduplicatingExecutionEventStore)
	if !ok {
		return nil, false, fmt.Errorf("%s: execution event store does not support atomic deduplication", operation)
	}
	dedupeKey := mappedJournalDedupeKey(key)
	appended, isNew, err := deduplicatingStore.AppendExecutionEventIfAbsent(ctx, mapped, dedupeKey)
	if err == nil {
		s.markPersisted(identity, kind)
		if !isNew {
			return nil, false, nil
		}
		return appended, true, nil
	}
	firstErr := fmt.Errorf("%s: %w", operation, err)
	persisted, reconcileErr := s.journal.hasPersistedIdentity(ctx, key)
	if reconcileErr != nil {
		return nil, false, errors.Join(firstErr, fmt.Errorf("reconcile failed append: %w", reconcileErr))
	}
	if persisted {
		s.markPersisted(identity, kind)
		return nil, false, nil
	}

	appended, isNew, err = deduplicatingStore.AppendExecutionEventIfAbsent(ctx, mapped, dedupeKey)
	if err == nil {
		s.markPersisted(identity, kind)
		if !isNew {
			return nil, false, nil
		}
		return appended, true, nil
	}
	retryErr := fmt.Errorf("%s retry: %w", operation, err)
	persisted, reconcileErr = s.journal.hasPersistedIdentity(ctx, key)
	if reconcileErr != nil {
		return nil, false, errors.Join(firstErr, retryErr, fmt.Errorf("reconcile failed append retry: %w", reconcileErr))
	}
	if persisted {
		s.markPersisted(identity, kind)
		return nil, false, nil
	}
	return nil, false, errors.Join(firstErr, retryErr)
}

func (s *State) markPersisted(identity MappedUpdateIdentity, kind mappedJournalRecordKind) {
	s.markProcessed(identity)
	switch kind {
	case mappedJournalRecordAssistantTranscript:
		s.assistantTranscriptSequence = identity.Sequence
	case mappedJournalRecordToolStreamClosure:
		s.rememberToolClosure(identity.Sequence)
	case mappedJournalRecordTerminalUsage:
		s.terminalUsageSequence = identity.Sequence
	case mappedJournalRecordPromptAccepted:
		s.promptAcceptedSequence = identity.Sequence
	case mappedJournalRecordPromptTerminal:
		s.promptTerminalSequence = identity.Sequence
	}
}

func (j Journal) hasPersistedIdentity(ctx context.Context, key string) (bool, error) {
	mapCtx := j.MapContext.normalized()
	var afterSeq int64
	for {
		listed, err := j.EventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  mapCtx.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   mapCtx.StreamID,
			AfterSeq:   afterSeq,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return false, fmt.Errorf("list mapped harness v2 updates: %w", err)
		}
		if len(listed) == 0 {
			return false, nil
		}
		nextSeq := afterSeq
		for _, event := range listed {
			if event.Seq > nextSeq {
				nextSeq = event.Seq
			}
			if _, eventKey, ok := mappedExecutionEventKey(event); ok && eventKey == key {
				return true, nil
			}
		}
		if len(listed) < store.MaxExecutionEventLimit {
			return false, nil
		}
		if nextSeq <= afterSeq {
			return false, fmt.Errorf("mapped harness v2 update scan did not advance after sequence %d", afterSeq)
		}
		afterSeq = nextSeq
	}
}

func (s *State) aggregateToolUpdate(event harnessv2.Event, sequence uint64) bool {
	if event.Update == nil || event.Update.ToolCall == nil {
		return false
	}
	if sequence <= s.aggregatedSequence {
		return false
	}
	s.aggregatedSequence = sequence
	tool := event.Update.ToolCall
	toolID := tool.ToolCallID
	accumulator := s.toolText[toolID]
	if accumulator == nil {
		if len(s.toolText) >= maxOpenToolAccumulators {
			if tool.Status == harnessv2.ToolCallStatusCompleted || tool.Status == harnessv2.ToolCallStatusFailed {
				return false
			}
			s.rememberOverflowTool(toolID)
			return true
		}
		accumulator = &streamText{}
		if s.consumeOverflowTool(toolID) {
			accumulator.omitForOverflow()
		}
		s.toolText[toolID] = accumulator
	}
	beforeBytes := accumulator.text.Len()
	applyToolUpdateToAccumulator(accumulator, event)
	s.toolBufferedBytes += accumulator.text.Len() - beforeBytes
	if s.toolBufferedBytes > maxBufferedToolContentBytes {
		bufferedBytes := accumulator.text.Len()
		accumulator.omitForOverflow()
		s.toolBufferedBytes -= bufferedBytes
	}
	return false
}

func applyToolUpdateToAccumulator(accumulator *streamText, event harnessv2.Event) {
	if accumulator == nil || event.Update == nil || event.Update.ToolCall == nil {
		return
	}
	tool := event.Update.ToolCall
	accumulator.lastEvent = event
	accumulator.hasEvent = true
	if strings.TrimSpace(tool.Title) != "" {
		accumulator.title = tool.Title
	}
	if strings.TrimSpace(tool.Kind) != "" {
		accumulator.kind = tool.Kind
	}
	content, multipleBlocks := toolContentFragment(tool.Content)
	if tool.ContentOmitted {
		accumulator.omitForOverflow()
	} else if multipleBlocks {
		accumulator.omitMultipleBlocks()
	} else if tool.ContentReplace {
		accumulator.replace(content)
	} else if len(tool.Content) > 0 {
		accumulator.append(content)
	}
}

func (s *State) finishToolUpdate(event harnessv2.Event) (string, bool, bool, string, string, bool) {
	if event.Update == nil || event.Update.ToolCall == nil {
		return "", false, false, "", "", false
	}
	tool := event.Update.ToolCall
	if tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed {
		return "", false, false, "", "", false
	}
	accumulator, ok := s.toolText[tool.ToolCallID]
	if !ok {
		if s.consumeOverflowTool(tool.ToolCallID) {
			if tool.ContentReplace && !tool.ContentOmitted {
				return "", false, false, "", "", false
			}
			return "", true, false, "", "", true
		}
		return "", false, false, "", "", false
	}
	s.removeToolAccumulator(tool.ToolCallID)
	return accumulator.text.String(), accumulator.overflow, accumulator.multipleBlocksOmitted,
		accumulator.title, accumulator.kind, true
}

func (s *State) rememberOverflowTool(toolID string) {
	if _, ok := s.overflowToolIDs[toolID]; ok {
		return
	}
	if len(s.overflowToolIDs) < maxOpenToolAccumulators {
		s.overflowToolIDs[toolID] = struct{}{}
		return
	}
	// Once the bounded exact set is full, conservatively mark otherwise
	// untracked terminal tools as omitted for the rest of this prompt.
	s.untrackedOverflowTool = true
}

func (s *State) consumeOverflowTool(toolID string) bool {
	if _, ok := s.overflowToolIDs[toolID]; ok {
		delete(s.overflowToolIDs, toolID)
		return true
	}
	return s.untrackedOverflowTool
}

func transientTerminalToolUpdate(event harnessv2.Event) (string, bool, bool, string, string, bool) {
	if event.Update == nil || event.Update.ToolCall == nil {
		return "", false, false, "", "", false
	}
	tool := event.Update.ToolCall
	if (tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed) ||
		(!tool.ContentOmitted && !tool.ContentReplace && len(tool.Content) == 0) {
		return "", false, false, "", "", false
	}
	accumulator := &streamText{}
	applyToolUpdateToAccumulator(accumulator, event)
	return accumulator.text.String(), accumulator.overflow, accumulator.multipleBlocksOmitted,
		accumulator.title, accumulator.kind, true
}

func (s *State) removeToolAccumulator(toolID string) {
	accumulator, ok := s.toolText[toolID]
	if !ok {
		return
	}
	delete(s.toolText, toolID)
	s.toolBufferedBytes -= accumulator.text.Len()
}

func withBufferedToolMetadata(event harnessv2.Event, title, kind string) harnessv2.Event {
	if event.Update == nil || event.Update.ToolCall == nil {
		return event
	}
	update := *event.Update
	tool := *event.Update.ToolCall
	if strings.TrimSpace(tool.Title) == "" {
		tool.Title = title
	}
	if strings.TrimSpace(tool.Kind) == "" {
		tool.Kind = kind
	}
	update.ToolCall = &tool
	event.Update = &update
	return event
}

func withInterruptedToolClosure(event harnessv2.Event, title, kind string) harnessv2.Event {
	event = withBufferedToolMetadata(event, title, kind)
	if event.Update == nil || event.Update.ToolCall == nil {
		return event
	}
	update := *event.Update
	tool := *event.Update.ToolCall
	tool.Status = harnessv2.ToolCallStatusFailed
	update.ToolCall = &tool
	event.Update = &update
	return event
}

func toolContentFragment(blocks []harnessv2.ContentBlock) (string, bool) {
	var content string
	projectedBlocks := 0
	for _, block := range blocks {
		var value string
		switch block.Type {
		case harnessv2.ContentBlockText:
			value = block.Text
		case harnessv2.ContentBlockResourceLink:
			resource := safeResourceDisplayName(block.Name)
			if resource == "" {
				resource = safeResourceDisplayURI(block.URI)
			}
			if resource != "" {
				value = "resource: " + resource
			}
		case harnessv2.ContentBlockArtifactRef:
			if block.Artifact != nil {
				value = "artifact: " + string(block.Artifact.ArtifactID)
			}
		}
		if value == "" {
			continue
		}
		projectedBlocks++
		if projectedBlocks > 1 {
			return "", true
		}
		content = value
	}
	return content, false
}

func safeResourceDisplayName(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil {
		if suffix := strings.IndexAny(value, "?#"); suffix >= 0 {
			return strings.TrimSpace(value[:suffix])
		}
		return value
	}
	if parsed.IsAbs() {
		return safeResourceDisplayURI(value)
	}
	if parsed.User != nil || parsed.Opaque != "" {
		return ""
	}
	if parsed.Host != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		if suffix := strings.IndexAny(value, "?#"); suffix >= 0 {
			value = strings.TrimSpace(value[:suffix])
		}
	}
	return value
}

func safeResourceDisplayURI(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() || parsed.User != nil || parsed.Opaque != "" {
		return ""
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}
