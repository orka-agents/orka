/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package liveevents

import (
	"errors"
	"testing"
)

func TestBrokerDeliversInOrderToTheTasksSubscribers(t *testing.T) {
	b := NewBroker(8)
	sub := b.Subscribe("ns", "task")
	other := b.Subscribe("ns", "other")
	defer other.Close()

	for _, id := range []string{"a", "b", "c"} {
		b.Publish(Event{Namespace: "ns", Task: "task", TaskUID: id})
	}
	sub.Close()
	var got []string
	for event := range sub.C() {
		got = append(got, event.TaskUID)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("events = %v, want a b c", got)
	}
	if sub.Err() != nil {
		t.Fatalf("Err() = %v after Close, want nil", sub.Err())
	}
	select {
	case event := <-other.C():
		t.Fatalf("another Task's subscriber received %+v", event)
	default:
	}
}

func TestBrokerDropsALaggingSubscriberInsteadOfSkippingEvents(t *testing.T) {
	b := NewBroker(2)
	sub := b.Subscribe("ns", "task")
	for range 3 {
		b.Publish(Event{Namespace: "ns", Task: "task"})
	}
	count := 0
	for range sub.C() {
		count++
	}
	if count != 2 {
		t.Fatalf("delivered %d events before the drop, want 2", count)
	}
	if !errors.Is(sub.Err(), ErrLagged) {
		t.Fatalf("Err() = %v, want ErrLagged", sub.Err())
	}
	// The dropped subscriber is forgotten; publishing again must not panic.
	b.Publish(Event{Namespace: "ns", Task: "task"})
	sub.Close()
}

func TestNilBrokerIsSafe(t *testing.T) {
	var b *Broker
	b.Publish(Event{Namespace: "ns", Task: "task"})
	sub := b.Subscribe("ns", "task")
	sub.Close()
	if _, open := <-sub.C(); open {
		t.Fatal("subscription of a nil broker delivered an event")
	}
}
