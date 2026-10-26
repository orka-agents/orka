package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestListSessionsPageCursorsInSQL(t *testing.T) {
	s := newAgentExecutionTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	// Created oldest first; "c-chat" and "d-chat" share a creation time, and
	// "e-chat" is written in another zone to check UTC normalization.
	for _, session := range []struct {
		name      string
		createdAt time.Time
	}{
		{"e-chat", base.In(time.FixedZone("PDT", -7*3600))},
		{"a-chat", base.Add(time.Minute)},
		{"b-gateway", base.Add(2 * time.Minute)},
		{"d-chat", base.Add(3*time.Minute + 500*time.Millisecond)},
		{"c-chat", base.Add(3*time.Minute + 500*time.Millisecond)},
		{"f-chat", base.Add(4 * time.Minute)},
	} {
		kind := "chat"
		if session.name == "b-gateway" {
			kind = store.SessionTypeGateway
		}
		if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "ns", Name: session.name, SessionType: kind, CreatedAt: session.createdAt, UpdatedAt: session.createdAt}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, &store.SessionRecord{Namespace: "other", Name: "aa-chat", SessionType: "chat", CreatedAt: base, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}

	var names []string
	var after *store.SessionPageCursor
	for range 3 {
		page, more, err := s.ListSessionsPage(ctx, "ns", after, 2, store.SessionTypeGateway)
		if err != nil {
			t.Fatal(err)
		}
		for _, session := range page {
			names = append(names, session.Name)
		}
		if !more {
			break
		}
		last := page[len(page)-1]
		after = &store.SessionPageCursor{CreatedAt: last.CreatedAt, Name: last.Name}
	}
	want := []string{"f-chat", "c-chat", "d-chat", "a-chat", "e-chat"}
	if len(names) != len(want) {
		t.Fatalf("paged sessions = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("paged sessions = %v, want %v", names, want)
		}
	}
	if page, _, err := s.ListSessionsPage(ctx, "ns", nil, 10, ""); err != nil || len(page) != 6 || page[3].Name != "b-gateway" {
		t.Fatalf("unfiltered page = %+v err=%v", page, err)
	}
}
