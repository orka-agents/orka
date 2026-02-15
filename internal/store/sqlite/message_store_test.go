package sqlite

import (
	"context"
	"testing"

	"github.com/sozercan/orka/internal/store"
)

func TestMessageStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	t.Run("send and get direct message", func(t *testing.T) {
		msg := &store.Message{
			Namespace:  "ns1",
			FromTask:   "task-a",
			ToTask:     "task-b",
			ParentTask: "coordinator",
			Content:    "hello from A",
		}
		if err := s.SendMessage(ctx, msg); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		msgs, err := s.GetMessages(ctx, "ns1", "task-b", "coordinator", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if msgs[0].Content != "hello from A" {
			t.Errorf("content = %q, want %q", msgs[0].Content, "hello from A")
		}
		if msgs[0].FromTask != "task-a" {
			t.Errorf("fromTask = %q, want %q", msgs[0].FromTask, "task-a")
		}
	})

	t.Run("broadcast message to all siblings", func(t *testing.T) {
		msg := &store.Message{
			Namespace:  "ns1",
			FromTask:   "task-c",
			ToTask:     "*",
			ParentTask: "coordinator",
			Content:    "broadcast msg",
		}
		if err := s.SendMessage(ctx, msg); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		// task-b should see the broadcast (same parent, different sender)
		msgs, err := s.GetMessages(ctx, "ns1", "task-b", "coordinator", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		found := false
		for _, m := range msgs {
			if m.Content == "broadcast msg" && m.FromTask == "task-c" {
				found = true
			}
		}
		if !found {
			t.Error("task-b did not receive broadcast from task-c")
		}

		// task-c should NOT see its own broadcast
		msgs, err = s.GetMessages(ctx, "ns1", "task-c", "coordinator", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		for _, m := range msgs {
			if m.Content == "broadcast msg" && m.FromTask == "task-c" {
				t.Error("sender received its own broadcast message")
			}
		}
	})

	t.Run("mark read prevents re-reading", func(t *testing.T) {
		msg := &store.Message{
			Namespace:  "ns2",
			FromTask:   "sender",
			ToTask:     "reader",
			ParentTask: "parent",
			Content:    "read me once",
		}
		if err := s.SendMessage(ctx, msg); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		// First read with markRead=true
		msgs, err := s.GetMessages(ctx, "ns2", "reader", "parent", true)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}

		// Second read should return nothing
		msgs, err = s.GetMessages(ctx, "ns2", "reader", "parent", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages after markRead, want 0", len(msgs))
		}
	})

	t.Run("different parent scopes messages", func(t *testing.T) {
		msg := &store.Message{
			Namespace:  "ns3",
			FromTask:   "worker-x",
			ToTask:     "*",
			ParentTask: "parent-1",
			Content:    "scoped msg",
		}
		if err := s.SendMessage(ctx, msg); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		// Worker in parent-2 should NOT see messages from parent-1
		msgs, err := s.GetMessages(ctx, "ns3", "worker-y", "parent-2", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages from different parent, want 0", len(msgs))
		}
	})

	t.Run("delete task messages", func(t *testing.T) {
		msg := &store.Message{
			Namespace:  "ns4",
			FromTask:   "del-sender",
			ToTask:     "del-receiver",
			ParentTask: "del-parent",
			Content:    "will be deleted",
		}
		if err := s.SendMessage(ctx, msg); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		if err := s.DeleteTaskMessages(ctx, "ns4", "del-sender"); err != nil {
			t.Fatalf("DeleteTaskMessages: %v", err)
		}

		msgs, err := s.GetMessages(ctx, "ns4", "del-receiver", "del-parent", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages after delete, want 0", len(msgs))
		}
	})

	t.Run("delete parent messages", func(t *testing.T) {
		for _, m := range []*store.Message{
			{Namespace: "ns5", FromTask: "child-1", ToTask: "child-2", ParentTask: "parent-del", Content: "msg1"},
			{Namespace: "ns5", FromTask: "child-2", ToTask: "*", ParentTask: "parent-del", Content: "msg2"},
		} {
			if err := s.SendMessage(ctx, m); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
		}

		if err := s.DeleteParentMessages(ctx, "ns5", "parent-del"); err != nil {
			t.Fatalf("DeleteParentMessages: %v", err)
		}

		msgs, err := s.GetMessages(ctx, "ns5", "child-1", "parent-del", false)
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Errorf("got %d messages after parent delete, want 0", len(msgs))
		}
	})
}
