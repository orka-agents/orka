package eventjournal

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// ErrToolBufferLimitExceeded reports that one prompt exceeded the bounded
// in-memory aggregation budget for streamed tool output.
var ErrToolBufferLimitExceeded = errors.New("harness v2 tool aggregation buffer limit exceeded")

// Journal owns duplicate-safe persistence of mapped harness v2 updates.
type Journal struct {
	EventStore store.ExecutionEventStore
	MapContext MapContext
}

// State is the mutable aggregation state for one non-reconnectable prompt
// stream. Public lifecycle/telemetry updates are durable as they arrive.
// Untrusted assistant/tool text becomes durable only after its logical stream
// ends, so redaction can see credential shapes split across protocol frames.
// Controller takeover deliberately terminalizes accepted prompts as
// OutcomeUnknown rather than replaying or persisting a raw-content crash buffer.
type State struct {
	journal           Journal
	keys              map[string]struct{}
	persistedKeys     map[string]struct{}
	aggregatedKeys    map[string]struct{}
	toolText          map[string]*streamText
	toolBufferedBytes int
	bufferErr         error
}

type streamText struct {
	text     strings.Builder
	runes    int
	overflow bool
	title    string
	kind     string
}

func (s *streamText) append(value string) {
	if s == nil || s.overflow || value == "" {
		return
	}
	runes := utf8.RuneCountInString(value)
	remaining := executionevents.MaxExecutionEventContentTextChars - s.runes
	if runes > remaining {
		// A truncated prefix cannot be redacted safely when a credential spans
		// the cutoff. Discard all buffered text and persist only truncation
		// metadata when the tool reaches a terminal update.
		s.text.Reset()
		s.runes = 0
		s.overflow = true
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
	s.append(value)
}

// HasUpdate reports whether event was already persisted or appended during
// this journal pass.
func (s *State) HasUpdate(event harnessv2.Event) bool {
	if s == nil {
		return false
	}
	_, ok := s.keys[mappedUpdateIdentity(event).Key()]
	return ok
}

// Open loads all persisted harness v2 update identities for this task stream.
func (j Journal) Open(ctx context.Context) (*State, error) {
	if j.EventStore == nil {
		return nil, fmt.Errorf("execution event store is required")
	}
	if err := j.MapContext.validate(); err != nil {
		return nil, err
	}
	keys := map[string]struct{}{}
	persistedKeys := map[string]struct{}{}
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
			return nil, fmt.Errorf("list mapped harness v2 updates: %w", err)
		}
		if len(listed) == 0 {
			break
		}
		for _, event := range listed {
			if event.Seq > afterSeq {
				afterSeq = event.Seq
			}
			if identity, ok := MappedUpdateIdentityFromEvent(event); ok {
				key := identity.Key()
				keys[key] = struct{}{}
				persistedKeys[key] = struct{}{}
			}
		}
		if len(listed) < store.MaxExecutionEventLimit {
			break
		}
	}
	return &State{
		journal:        j,
		keys:           keys,
		persistedKeys:  persistedKeys,
		aggregatedKeys: map[string]struct{}{},
		toolText:       map[string]*streamText{},
	}, nil
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
	if s.bufferErr != nil {
		return nil, false, s.bufferErr
	}
	key := mappedUpdateIdentity(event).Key()
	mapped, err := MapUpdate(event, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	if err := s.aggregateToolUpdate(event, key); err != nil {
		return nil, false, err
	}
	if s.HasUpdate(event) {
		s.finishToolUpdate(event)
		return nil, false, nil
	}
	if event.Update.Kind == harnessv2.UpdateAssistantMessageChunk {
		// The complete terminal transcript is persisted once redaction can see
		// all chunk boundaries. Retain the chunk key only for same-pass replay.
		s.keys[key] = struct{}{}
		return nil, false, nil
	}
	if isContentOnlyToolFragment(event) {
		// Keep streamed output in the accumulator, but do not emit an unbounded
		// series of identical ToolCallStarted events. The terminal tool event
		// receives the complete, redacted text.
		s.keys[key] = struct{}{}
		return nil, false, nil
	}
	if contentText, truncated, title, kind, ok := s.finishToolUpdate(event); ok {
		mappedEvent := withBufferedToolMetadata(event, title, kind)
		mapped, err = mapToolUpdateWithContent(mappedEvent, s.journal.MapContext, contentText, truncated)
		if err != nil {
			return nil, false, err
		}
	} else if isNonTerminalToolUpdate(event) {
		mapped, err = mapToolUpdateWithoutMetadata(event, s.journal.MapContext)
		if err != nil {
			return nil, false, err
		}
	}
	return s.appendMappedEvent(ctx, key, mapped, "append mapped harness v2 update")
}

func isContentOnlyToolFragment(event harnessv2.Event) bool {
	if event.Update == nil || event.Update.Kind != harnessv2.UpdateToolCallUpdate || event.Update.ToolCall == nil {
		return false
	}
	tool := event.Update.ToolCall
	if (!tool.ContentReplace && len(tool.Content) == 0) ||
		strings.TrimSpace(tool.Title) != "" || strings.TrimSpace(tool.Kind) != "" {
		return false
	}
	return tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed
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
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	if transcript == "" {
		return nil, false, nil
	}
	key := mappedUpdateIdentity(terminal).Key()
	if _, ok := s.persistedKeys[key]; ok {
		return nil, false, nil
	}
	mapped, err := MapAssistantTranscript(terminal, s.journal.MapContext, transcript)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(ctx, key, mapped, "append mapped harness v2 assistant transcript")
}

// AppendAssistantStreamClosureIfNew persists the complete assistant text seen
// before a non-terminal stream closure. The last assistant update supplies the
// durable protocol identity; the complete buffered text supplies the redaction
// boundary that individual chunks cannot provide safely.
func (s *State) AppendAssistantStreamClosureIfNew(
	ctx context.Context,
	lastUpdate harnessv2.Event,
	transcript string,
) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	if transcript == "" {
		return nil, false, nil
	}
	if lastUpdate.Type != harnessv2.EventUpdate || lastUpdate.Update == nil || lastUpdate.Update.AssistantMessage == nil {
		return nil, false, fmt.Errorf("last assistant update is required")
	}
	key := mappedUpdateIdentity(lastUpdate).Key()
	if _, ok := s.persistedKeys[key]; ok {
		return nil, false, nil
	}
	mapped, err := MapAssistantTranscript(lastUpdate, s.journal.MapContext, transcript)
	if err != nil {
		return nil, false, err
	}
	return s.appendMappedEvent(ctx, key, mapped, "append mapped harness v2 assistant stream closure")
}

// appendMappedEvent reconciles an append error against the durable stream
// before retrying. The execution-event store does not expose an idempotency
// key, so an error may be returned after the event committed. Readback avoids
// duplicating that ambiguous write; one retry is allowed only after absence is
// confirmed, and a second readback reconciles an ambiguous retry.
func (s *State) appendMappedEvent(
	ctx context.Context,
	key string,
	mapped *store.ExecutionEvent,
	operation string,
) (*store.ExecutionEvent, bool, error) {
	appended, err := s.journal.EventStore.AppendExecutionEvent(ctx, mapped)
	if err == nil {
		s.markPersisted(key)
		return appended, true, nil
	}
	firstErr := fmt.Errorf("%s: %w", operation, err)
	persisted, reconcileErr := s.journal.hasPersistedIdentity(ctx, key)
	if reconcileErr != nil {
		return nil, false, errors.Join(firstErr, fmt.Errorf("reconcile failed append: %w", reconcileErr))
	}
	if persisted {
		s.markPersisted(key)
		return nil, false, nil
	}

	appended, err = s.journal.EventStore.AppendExecutionEvent(ctx, mapped)
	if err == nil {
		s.markPersisted(key)
		return appended, true, nil
	}
	retryErr := fmt.Errorf("%s retry: %w", operation, err)
	persisted, reconcileErr = s.journal.hasPersistedIdentity(ctx, key)
	if reconcileErr != nil {
		return nil, false, errors.Join(firstErr, retryErr, fmt.Errorf("reconcile failed append retry: %w", reconcileErr))
	}
	if persisted {
		s.markPersisted(key)
		return nil, false, nil
	}
	return nil, false, errors.Join(firstErr, retryErr)
}

func (s *State) markPersisted(key string) {
	s.keys[key] = struct{}{}
	s.persistedKeys[key] = struct{}{}
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
			if identity, ok := MappedUpdateIdentityFromEvent(event); ok && identity.Key() == key {
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

func (s *State) aggregateToolUpdate(event harnessv2.Event, key string) error {
	if event.Update == nil || event.Update.ToolCall == nil {
		return nil
	}
	if _, ok := s.aggregatedKeys[key]; ok {
		return nil
	}
	s.aggregatedKeys[key] = struct{}{}
	toolID := event.Update.ToolCall.ToolCallID
	accumulator := s.toolText[toolID]
	if accumulator == nil {
		if len(s.toolText) >= maxOpenToolAccumulators {
			s.bufferErr = fmt.Errorf(
				"%w: open tool streams exceed %d", ErrToolBufferLimitExceeded, maxOpenToolAccumulators,
			)
			return s.bufferErr
		}
		accumulator = &streamText{}
		s.toolText[toolID] = accumulator
	}
	beforeBytes := accumulator.text.Len()
	tool := event.Update.ToolCall
	if strings.TrimSpace(tool.Title) != "" {
		accumulator.title = tool.Title
	}
	if strings.TrimSpace(tool.Kind) != "" {
		accumulator.kind = tool.Kind
	}
	if tool.ContentReplace {
		accumulator.replace(toolContentFragment(tool.Content))
	} else if len(tool.Content) > 0 {
		accumulator.append(toolContentFragment(tool.Content))
	}
	s.toolBufferedBytes += accumulator.text.Len() - beforeBytes
	if s.toolBufferedBytes > maxBufferedToolContentBytes {
		s.bufferErr = fmt.Errorf(
			"%w: buffered tool content exceeds %d bytes", ErrToolBufferLimitExceeded, maxBufferedToolContentBytes,
		)
		return s.bufferErr
	}
	return nil
}

func (s *State) finishToolUpdate(event harnessv2.Event) (string, bool, string, string, bool) {
	if event.Update == nil || event.Update.ToolCall == nil {
		return "", false, "", "", false
	}
	tool := event.Update.ToolCall
	if tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed {
		return "", false, "", "", false
	}
	accumulator, ok := s.toolText[tool.ToolCallID]
	if !ok {
		return "", false, "", "", false
	}
	delete(s.toolText, tool.ToolCallID)
	s.toolBufferedBytes -= accumulator.text.Len()
	return accumulator.text.String(), accumulator.overflow, accumulator.title, accumulator.kind, true
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

func toolContentFragment(blocks []harnessv2.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		var value string
		switch block.Type {
		case harnessv2.ContentBlockText:
			value = block.Text
		case harnessv2.ContentBlockResourceLink:
			resource := strings.TrimSpace(block.Name)
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
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(value)
	}
	return text.String()
}

func safeResourceDisplayURI(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() || parsed.User != nil {
		return ""
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}
