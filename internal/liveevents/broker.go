/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package liveevents fans out the events of running agent Tasks to consumers in
// the same process while the turn runs. It is a best-effort tap beside the
// durable execution event journal: a consumer that falls behind loses its
// subscription and must reload from the journal, and nothing here survives a
// restart or a change of leader.
package liveevents

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// DefaultBuffer is the number of events one subscriber may have pending.
const DefaultBuffer = 1024

// ErrLagged reports that a subscriber did not read fast enough and was dropped.
var ErrLagged = errors.New("live event subscriber fell behind")

// Event is one event of a running Task, published in the order the controller
// received it. Runtime content is redacted before it is published.
type Event struct {
	Namespace string
	Task      string
	TaskUID   string
	// Runtime is an event from the Task's prompt stream.
	Runtime *harnessv2.Event
	// Approval reports a permission request held for the Task's requester, or
	// how it ended.
	Approval *Approval
}

// ApprovalState is the state of a held permission request.
type ApprovalState string

const (
	// ApprovalPending waits for the Task's requester to choose an option.
	ApprovalPending ApprovalState = "Pending"
	// ApprovalResolved was answered with OptionID.
	ApprovalResolved ApprovalState = "Resolved"
	// ApprovalExpired was rejected because nobody answered in time.
	ApprovalExpired ApprovalState = "Expired"
	// ApprovalCancelled ended with the prompt, unanswered.
	ApprovalCancelled ApprovalState = "Cancelled"
)

// Approval is a permission request the controller holds for a human decision.
type Approval struct {
	// ID names the approval in the Task approval decision API.
	ID    string
	State ApprovalState
	// ToolCallID is the harness tool call the request is for, when known.
	ToolCallID string
	ToolName   string
	Title      string
	// Input is the input of the call the request is for, as a JSON value,
	// redacted, when the runtime reported one. It is never stored.
	Input     json.RawMessage
	Options   []harnessv2.PermissionOption
	ExpiresAt time.Time
	// OptionID is the chosen option of a resolved approval.
	OptionID string
}

type taskKey struct{ namespace, task string }

// Broker delivers published events to the subscribers of their Task. A nil
// Broker drops everything, so producers need no checks.
type Broker struct {
	buffer int

	mu   sync.Mutex
	subs map[taskKey]map[*Subscription]struct{}
}

// NewBroker builds a broker whose subscribers may each hold buffer pending
// events; a non-positive buffer selects DefaultBuffer.
func NewBroker(buffer int) *Broker {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Broker{buffer: buffer, subs: map[taskKey]map[*Subscription]struct{}{}}
}

// Subscription receives one Task's events from the moment it was created.
type Subscription struct {
	broker *Broker
	key    taskKey
	ch     chan Event

	once sync.Once
	err  error
}

// C delivers events until the subscription is closed or falls behind.
func (s *Subscription) C() <-chan Event { return s.ch }

// Err reports why C closed: ErrLagged, or nil after Close.
func (s *Subscription) Err() error {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	return s.err
}

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	s.closeLocked(nil)
}

func (s *Subscription) closeLocked(err error) {
	s.once.Do(func() {
		s.err = err
		if set := s.broker.subs[s.key]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(s.broker.subs, s.key)
			}
		}
		close(s.ch)
	})
}

// Subscribe starts receiving the events of one Task.
func (b *Broker) Subscribe(namespace, task string) *Subscription {
	if b == nil {
		b = NewBroker(1)
	}
	sub := &Subscription{broker: b, key: taskKey{namespace, task}, ch: make(chan Event, b.buffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[sub.key] == nil {
		b.subs[sub.key] = map[*Subscription]struct{}{}
	}
	b.subs[sub.key][sub] = struct{}{}
	return sub
}

// Publish delivers an event without blocking. A subscriber whose buffer is
// full is closed with ErrLagged instead of being skipped, so a consumer never
// sees a stream with a silent gap.
func (b *Broker) Publish(event Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs[taskKey{event.Namespace, event.Task}] {
		select {
		case sub.ch <- event:
		default:
			sub.closeLocked(ErrLagged)
		}
	}
}
