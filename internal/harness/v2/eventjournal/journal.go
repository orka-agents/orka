package eventjournal

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// Journal owns restart-safe persistence of mapped harness v2 updates.
type Journal struct {
	EventStore store.ExecutionEventStore
	MapContext MapContext
}

// State is the mutable deduplication state for one dispatcher pass.
type State struct {
	journal        Journal
	keys           map[string]struct{}
	aggregatedKeys map[string]struct{}
	toolText       map[string]*streamText
}

type streamText struct {
	text     strings.Builder
	runes    int
	overflow bool
}

func (s *streamText) append(value string) {
	if s == nil || s.overflow || value == "" {
		return
	}
	runes := utf8.RuneCountInString(value)
	if runes > executionevents.MaxExecutionEventContentTextChars-s.runes {
		s.text.Reset()
		s.runes = 0
		s.overflow = true
		return
	}
	s.text.WriteString(value)
	s.runes += runes
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
				keys[identity.Key()] = struct{}{}
			}
		}
		if len(listed) < store.MaxExecutionEventLimit {
			break
		}
	}
	return &State{
		journal:        j,
		keys:           keys,
		aggregatedKeys: map[string]struct{}{},
		toolText:       map[string]*streamText{},
	}, nil
}

// AppendUpdateIfNew maps and appends event unless its full protocol identity is
// already persisted or was processed earlier in this pass. Assistant chunks
// are retained only for same-pass deduplication; the terminal transcript is the
// sole durable assistant text event.
func (s *State) AppendUpdateIfNew(ctx context.Context, event harnessv2.Event) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	key := mappedUpdateIdentity(event).Key()
	mapped, err := MapUpdate(event, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	s.aggregateToolText(event, key)
	if s.HasUpdate(event) {
		s.finishToolText(event)
		return nil, false, nil
	}
	if event.Update.Kind == harnessv2.UpdateAssistantMessageChunk {
		// The complete terminal transcript is persisted once redaction can see
		// all chunk boundaries. Retain the chunk key only for same-pass replay.
		s.keys[key] = struct{}{}
		return nil, false, nil
	}
	if contentText, truncated, ok := s.finishToolText(event); ok {
		mapped, err = mapToolUpdateWithContent(event, s.journal.MapContext, contentText, truncated)
		if err != nil {
			return nil, false, err
		}
	}
	appended, err := s.journal.EventStore.AppendExecutionEvent(ctx, mapped)
	if err != nil {
		return nil, false, fmt.Errorf("append mapped harness v2 update: %w", err)
	}
	s.keys[key] = struct{}{}
	return appended, true, nil
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
	if _, ok := s.keys[key]; ok {
		return nil, false, nil
	}
	mapped, err := MapAssistantTranscript(terminal, s.journal.MapContext, transcript)
	if err != nil {
		return nil, false, err
	}
	appended, err := s.journal.EventStore.AppendExecutionEvent(ctx, mapped)
	if err != nil {
		return nil, false, fmt.Errorf("append mapped harness v2 assistant transcript: %w", err)
	}
	s.keys[key] = struct{}{}
	return appended, true, nil
}

func (s *State) aggregateToolText(event harnessv2.Event, key string) {
	if event.Update == nil || event.Update.ToolCall == nil || len(event.Update.ToolCall.Content) == 0 {
		return
	}
	if _, ok := s.aggregatedKeys[key]; ok {
		return
	}
	s.aggregatedKeys[key] = struct{}{}
	toolID := event.Update.ToolCall.ToolCallID
	accumulator := s.toolText[toolID]
	if accumulator == nil {
		accumulator = &streamText{}
		s.toolText[toolID] = accumulator
	}
	accumulator.append(toolContentFragment(event.Update.ToolCall.Content))
}

func (s *State) finishToolText(event harnessv2.Event) (string, bool, bool) {
	if event.Update == nil || event.Update.ToolCall == nil {
		return "", false, false
	}
	tool := event.Update.ToolCall
	if tool.Status != harnessv2.ToolCallStatusCompleted && tool.Status != harnessv2.ToolCallStatusFailed {
		return "", false, false
	}
	accumulator, ok := s.toolText[tool.ToolCallID]
	if !ok {
		return "", false, false
	}
	delete(s.toolText, tool.ToolCallID)
	return accumulator.text.String(), accumulator.overflow, true
}

func toolContentFragment(blocks []harnessv2.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		var value string
		switch block.Type {
		case harnessv2.ContentBlockText:
			value = block.Text
		case harnessv2.ContentBlockResourceLink:
			value = "resource: " + strings.TrimSpace(block.Name)
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
