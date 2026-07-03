package cliwrapper

import (
	"errors"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

var (
	errTurnAlreadyExists      = errors.New("turn already exists")
	errTurnAlreadyCompleted   = errors.New("turn already completed")
	errMaximumConcurrentTurns = errors.New("maximum concurrent turns reached")
)

// maxConsumedTurnIDs bounds the in-memory tombstone of consumed turn IDs.
const maxConsumedTurnIDs = 1024

// turnRegistry owns wrapper-local turn admission, lookup, active-turn accounting,
// and the bounded consumed-turn tombstone. It is intentionally process-local: the
// controller's persisted event-store check remains the cross-restart backstop.
type turnRegistry struct {
	mu          sync.RWMutex
	turns       map[harness.HarnessTurnID]*turnState
	activeTurns int
	// consumedTurns is a bounded set of turn IDs that have been accepted and then
	// evicted, so a duplicate StartTurn for an already-run-and-evicted turn is
	// rejected deterministically (409) instead of being re-executed. This closes
	// the at-least-once external side-effect window when the controller retries a
	// StartTurn whose "started" marker it failed to persist after the turn had
	// already completed and been evicted from active turns. Bounded FIFO so memory
	// is capped; it does NOT survive a wrapper process restart.
	consumedTurns map[harness.HarnessTurnID]struct{}
	consumedOrder []harness.HarnessTurnID
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{
		turns:         map[harness.HarnessTurnID]*turnState{},
		consumedTurns: map[harness.HarnessTurnID]struct{}{},
	}
}

func (r *turnRegistry) admit(request harness.StartTurnRequest, now func() time.Time) (*turnState, error) {
	state := newTurnState(request, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.turns[request.TurnID]; exists {
		return nil, errTurnAlreadyExists
	}
	if _, consumed := r.consumedTurns[request.TurnID]; consumed {
		return nil, errTurnAlreadyCompleted
	}
	if r.activeTurns >= 1 {
		return nil, errMaximumConcurrentTurns
	}
	r.turns[request.TurnID] = state
	r.activeTurns++
	return state, nil
}

func (r *turnRegistry) lookup(id harness.HarnessTurnID) *turnState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.turns[id]
}

func (r *turnRegistry) active(id harness.HarnessTurnID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.turns[id] != nil
}

func (r *turnRegistry) finishActive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeTurns > 0 {
		r.activeTurns--
	}
}

func (r *turnRegistry) evict(turn *turnState) {
	if turn == nil {
		return
	}
	r.mu.Lock()
	if current := r.turns[turn.id()]; current == turn {
		delete(r.turns, turn.id())
	}
	r.markConsumedLocked(turn.id())
	r.mu.Unlock()
}

// markConsumedLocked records a turn ID as consumed (accepted then evicted) in a
// bounded FIFO set. Caller must hold r.mu.
func (r *turnRegistry) markConsumedLocked(id harness.HarnessTurnID) {
	if _, ok := r.consumedTurns[id]; ok {
		return
	}
	r.consumedTurns[id] = struct{}{}
	r.consumedOrder = append(r.consumedOrder, id)
	for len(r.consumedOrder) > maxConsumedTurnIDs {
		oldest := r.consumedOrder[0]
		r.consumedOrder = r.consumedOrder[1:]
		delete(r.consumedTurns, oldest)
	}
}
