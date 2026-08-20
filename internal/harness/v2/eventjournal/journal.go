package eventjournal

import (
	"context"
	"fmt"

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
	journal Journal
	keys    map[string]struct{}
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
	return &State{journal: j, keys: keys}, nil
}

// AppendUpdateIfNew maps and appends event unless its full protocol identity is
// already persisted or was appended earlier in this pass.
func (s *State) AppendUpdateIfNew(ctx context.Context, event harnessv2.Event) (*store.ExecutionEvent, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("harness v2 journal state is required")
	}
	key := mappedUpdateIdentity(event).Key()
	if s.HasUpdate(event) {
		return nil, false, nil
	}
	mapped, err := MapUpdate(event, s.journal.MapContext)
	if err != nil {
		return nil, false, err
	}
	appended, err := s.journal.EventStore.AppendExecutionEvent(ctx, mapped)
	if err != nil {
		return nil, false, fmt.Errorf("append mapped harness v2 update: %w", err)
	}
	s.keys[key] = struct{}{}
	return appended, true, nil
}
