/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package liveevents

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const DefaultBuffer = 1024

var ErrLagged = errors.New("live event subscriber fell behind")

type Event struct {
	Namespace string
	Task      string
	TaskUID   string
	Runtime   *harnessv2.Event
	Approval  *Approval
}

type ApprovalState string

const (
	ApprovalPending   ApprovalState = "Pending"
	ApprovalResolved  ApprovalState = "Resolved"
	ApprovalExpired   ApprovalState = "Expired"
	ApprovalCancelled ApprovalState = "Cancelled"
)

type Approval struct {
	ID         string
	State      ApprovalState
	ToolCallID string
	ToolName   string
	Title      string
	Input      json.RawMessage
	Options    []harnessv2.PermissionOption
	ExpiresAt  time.Time
	OptionID   string
}

type taskKey struct{ namespace, task string }

type Broker struct {
	buffer int

	mu   sync.Mutex
	subs map[taskKey]map[*Subscription]struct{}
}

func NewBroker(buffer int) *Broker {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Broker{buffer: buffer, subs: map[taskKey]map[*Subscription]struct{}{}}
}

type Subscription struct {
	broker *Broker
	key    taskKey
	ch     chan Event

	once sync.Once
	err  error
}

func (s *Subscription) C() <-chan Event { return s.ch }

func (s *Subscription) Err() error {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	return s.err
}

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
