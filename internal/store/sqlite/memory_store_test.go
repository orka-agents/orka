package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/store"
)

func TestMemoryStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	memory := &store.Memory{
		Namespace:   "ns-mem",
		SessionName: "session-a",
		AgentName:   "agent-a",
		TaskName:    "task-a",
		ParentTask:  "parent-a",
		Source:      "remember_tool",
		Content:     "Prefer Postgres migrations for durable storage work.",
		Tags:        []string{"storage", "durability", "storage"},
	}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	if memory.ID == "" {
		t.Fatalf("CreateMemory did not assign ID")
	}

	got, err := s.GetMemory(ctx, "ns-mem", memory.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if got.Content != memory.Content || got.Namespace != "ns-mem" || got.Source != "remember_tool" {
		t.Fatalf("unexpected memory: %+v", got)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("expected compacted tags, got %+v", got.Tags)
	}

	listed, err := s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-mem", Query: "postgres", Tags: []string{"storage"}})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != memory.ID {
		t.Fatalf("ListMemories = %+v, want created memory", listed)
	}

	if err := s.MarkMemoriesRecalled(ctx, "ns-mem", []string{memory.ID}); err != nil {
		t.Fatalf("MarkMemoriesRecalled: %v", err)
	}
	got, err = s.GetMemory(ctx, "ns-mem", memory.ID)
	if err != nil {
		t.Fatalf("GetMemory after recall: %v", err)
	}
	if got.RecalledCount != 1 || got.LastRecalledAt == nil {
		t.Fatalf("recall stats not updated: %+v", got)
	}

	if err := s.SetMemoryDisabled(ctx, "ns-mem", memory.ID, true); err != nil {
		t.Fatalf("SetMemoryDisabled: %v", err)
	}
	listed, err = s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-mem", Query: "postgres"})
	if err != nil {
		t.Fatalf("ListMemories disabled hidden: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("disabled memory should be hidden, got %+v", listed)
	}
	listed, err = s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-mem", Query: "postgres", IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListMemories include disabled: %v", err)
	}
	if len(listed) != 1 || !listed[0].Disabled {
		t.Fatalf("expected disabled memory when included, got %+v", listed)
	}

	if err := s.DeleteMemory(ctx, "ns-mem", memory.ID); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}
	listed, err = s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-mem", IncludeDisabled: true})
	if err != nil {
		t.Fatalf("ListMemories after delete: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("deleted memory should be hidden, got %+v", listed)
	}
	listed, err = s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-mem", IncludeDisabled: true, IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListMemories include deleted: %v", err)
	}
	if len(listed) != 1 || !listed[0].Deleted {
		t.Fatalf("expected soft-deleted memory when included, got %+v", listed)
	}

	if err := s.DeleteMemory(ctx, "ns-mem", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteMemory missing error = %v, want ErrNotFound", err)
	}
}

func TestMemoryProposalStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	proposal := &store.MemoryProposal{
		Namespace:   "ns-prop",
		TaskName:    "task-a",
		AgentName:   "agent-a",
		Type:        "skill",
		SkillName:   "sqlite-memory",
		Title:       "Add SQLite memory helper",
		Description: "Capture a reusable SQLite migration pattern.",
		Content:     "When adding store tables, keep migrations idempotent and covered by tests.",
		Patch:       "diff --git a/skills/sqlite.md b/skills/sqlite.md",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if proposal.ID == "" {
		t.Fatalf("CreateMemoryProposal did not assign ID")
	}

	listed, err := s.ListMemoryProposals(ctx, store.MemoryProposalFilter{Namespace: "ns-prop", Status: "pending", Query: "sqlite"})
	if err != nil {
		t.Fatalf("ListMemoryProposals: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != proposal.ID || listed[0].Status != "pending" {
		t.Fatalf("unexpected proposals: %+v", listed)
	}

	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace:  "ns-prop",
		ID:         proposal.ID,
		Status:     "accepted",
		Reviewer:   "maintainer",
		ReviewNote: "Looks useful.",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}
	got, err := s.GetMemoryProposal(ctx, "ns-prop", proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	if got.Status != "accepted" || got.Reviewer != "maintainer" || got.ReviewedAt == nil {
		t.Fatalf("review not persisted: %+v", got)
	}

	if err := s.ArchiveMemoryProposal(ctx, "ns-prop", proposal.ID); err != nil {
		t.Fatalf("ArchiveMemoryProposal: %v", err)
	}
	got, err = s.GetMemoryProposal(ctx, "ns-prop", proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal archived: %v", err)
	}
	if got.Status != "archived" {
		t.Fatalf("archive status = %q, want archived", got.Status)
	}

	if _, err := s.GetMemoryProposal(ctx, "ns-prop", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetMemoryProposal missing error = %v, want ErrNotFound", err)
	}
}

func TestTranscriptSearch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	for _, name := range []string{"prior", "current"} {
		if err := s.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "ns-transcript",
			Name:        name,
			SessionType: "task",
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			t.Fatalf("CreateSession %s: %v", name, err)
		}
	}

	priorLong := strings.Repeat("prefix ", 80) + "needle migration details live here" + strings.Repeat(" suffix", 80)
	if err := s.AppendMessages(ctx, "ns-transcript", "prior", []store.SessionMessage{
		{Role: "user", Content: "unrelated setup", Timestamp: now},
		{Role: "assistant", Content: priorLong, Timestamp: now.Add(time.Second)},
	}); err != nil {
		t.Fatalf("AppendMessages prior: %v", err)
	}
	if err := s.AppendMessages(ctx, "ns-transcript", "current", []store.SessionMessage{
		{Role: "assistant", Content: "needle from the current active session should be excluded", Timestamp: now.Add(2 * time.Second)},
	}); err != nil {
		t.Fatalf("AppendMessages current: %v", err)
	}

	results, err := s.SearchTranscript(ctx, store.TranscriptSearchFilter{
		Namespace:          "ns-transcript",
		Query:              "needle",
		ExcludeSessionName: "current",
		Limit:              5,
		MaxSnippetLength:   90,
	})
	if err != nil {
		t.Fatalf("SearchTranscript: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}
	if results[0].SessionName != "prior" || results[0].Role != "assistant" {
		t.Fatalf("unexpected result: %+v", results[0])
	}
	if !strings.Contains(results[0].Snippet, "needle") {
		t.Fatalf("snippet missing search term: %q", results[0].Snippet)
	}
	if len([]rune(results[0].Snippet)) > 92 { // allow ellipsis on both sides
		t.Fatalf("snippet too long: %d %q", len([]rune(results[0].Snippet)), results[0].Snippet)
	}
}
