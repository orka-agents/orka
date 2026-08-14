package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	sqlite3 "modernc.org/sqlite/lib"
)

const memoryTestTaskA = "task-a"

func TestMemoryStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	memory := &store.Memory{
		Namespace:   "ns-mem",
		SessionName: "session-a",
		AgentName:   "agent-a",
		TaskName:    memoryTestTaskA,
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

func TestSetLegacyMemoryDisabledWithAuditIsAtomic(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	memory := &store.Memory{Namespace: "ns-governance", Source: "api", Content: "durable guidance"}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: memory.Namespace, NamespaceUID: "ns-governance-uid", Actor: "test",
		Action: "memory.test", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	governedMemoryQuotas.NamespaceAuditRows = 1
	governedMemoryQuotas.GlobalAuditRows = 100

	err := s.SetLegacyMemoryDisabledWithAudit(
		ctx, memory.Namespace, "ns-governance-uid", memory.ID, true,
		"alice", "local memory governance change", "request-a", now.Add(time.Second),
	)
	if !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("SetLegacyMemoryDisabledWithAudit() error = %v, want ErrCapacity", err)
	}
	unchanged, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Disabled {
		t.Fatalf("memory disabled after rolled-back audit failure: %#v", unchanged)
	}

	governedMemoryQuotas.NamespaceAuditRows = 10
	if err := s.SetLegacyMemoryDisabledWithAudit(
		ctx, memory.Namespace, "ns-governance-uid", memory.ID, true,
		"alice", "local memory governance change", "request-b", now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Disabled {
		t.Fatalf("memory not disabled after atomic governance change: %#v", updated)
	}
	audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{
		NamespaceUID: "ns-governance-uid", MemoryID: memory.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Action != "memory.disable" ||
		audits[0].PreviousState != "disabled=false" || audits[0].NewState != "disabled=true" {
		t.Fatalf("disable audits = %#v", audits)
	}
}

func TestUpdateLegacyMemoryWithAuditPreservesReviewedNoOp(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 7, 0, 0, 0, time.UTC)
	proposal := &store.MemoryProposal{
		Namespace: "ns-legacy-noop", Type: proposalTypeMemory, Title: "reviewed", Content: "reviewed guidance",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeUpdatedAt := memory.UpdatedAt
	updated, err := s.UpdateLegacyMemoryWithAudit(
		ctx, memory, "ns-legacy-noop-uid", "operator", "legacy memory update", "request-noop", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Trust != store.MemoryTrustReviewed || updated.Source != memorySourceProposal ||
		updated.SourceProposalID != proposal.ID || updated.GovernanceRevision != 1 ||
		!updated.UpdatedAt.Equal(beforeUpdatedAt) {
		t.Fatalf("no-op legacy update changed reviewed governance: before=%+v after=%+v", memory, updated)
	}
	audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{
		NamespaceUID: "ns-legacy-noop-uid", MemoryID: memory.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 0 {
		t.Fatalf("no-op legacy update audits = %+v, want none", audits)
	}
}

func TestUpdateLegacyMemoryWithAuditPreservesTrustedNoOp(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 7, 10, 0, 0, time.UTC)
	memory := &store.Memory{Namespace: "ns-legacy-trusted-noop", Source: "manual", Content: "trusted guidance"}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: memory.Namespace, NamespaceUID: "ns-legacy-trusted-noop-uid", Actor: "operator", Action: "memory.trust",
		PreviousState: string(store.MemoryTrustUntrusted), NewState: string(store.MemoryTrustTrusted),
		MemoryID: memory.ID, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateLegacyMemoryWithAudit(
		ctx, before, "ns-legacy-trusted-noop-uid", "editor", "legacy memory update", "request-trusted-noop", now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Trust != store.MemoryTrustTrusted || updated.GovernanceRevision != 2 ||
		!updated.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("trusted no-op changed governance: before=%+v after=%+v", before, updated)
	}
	audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{
		NamespaceUID: "ns-legacy-trusted-noop-uid", MemoryID: memory.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].NewState != string(store.MemoryTrustTrusted) {
		t.Fatalf("trusted no-op audits = %+v", audits)
	}
}

func TestUpdateLegacyMemoryWithAuditDemotesTrustedProvenanceChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.Memory)
	}{
		{name: "session", mutate: func(memory *store.Memory) {
			memory.SessionName = "session-b" //nolint:goconst
		}},
		{name: "agent", mutate: func(memory *store.Memory) { memory.AgentName = "agent-b" }},
		{name: "task", mutate: func(memory *store.Memory) { memory.TaskName = "task-b" }},
		{name: "parent task", mutate: func(memory *store.Memory) { memory.ParentTask = "parent-b" }},
		{name: "source", mutate: func(memory *store.Memory) { memory.Source = "operator-edit" }},
		{name: "content", mutate: func(memory *store.Memory) { memory.Content = "changed guidance" }},
		{name: "tags", mutate: func(memory *store.Memory) { memory.Tags = []string{"changed"} }},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 8, 1, 7, 15, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute)
			namespace := "ns-legacy-provenance-" + strings.ReplaceAll(tt.name, " ", "-")
			namespaceUID := namespace + "-uid"
			memory := &store.Memory{
				Namespace: namespace, SessionName: "session-a", AgentName: "agent-a", TaskName: "task-a",
				ParentTask: "parent-a", Source: "manual", Content: "trusted guidance", Tags: []string{"stable"},
			}
			if err := s.CreateMemory(ctx, memory); err != nil {
				t.Fatal(err)
			}
			if err := s.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
				Namespace: namespace, NamespaceUID: namespaceUID, Actor: "operator", Action: "memory.trust",
				PreviousState: string(store.MemoryTrustUntrusted), NewState: string(store.MemoryTrustTrusted),
				MemoryID: memory.ID, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			desired, err := s.GetMemory(ctx, namespace, memory.ID)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(desired)
			updated, err := s.UpdateLegacyMemoryWithAudit(
				ctx, desired, namespaceUID, "editor", "legacy memory provenance changed", "request-change", now.Add(time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Trust != store.MemoryTrustUntrusted || updated.GovernanceRevision != 3 ||
				updated.SessionName != desired.SessionName || updated.AgentName != desired.AgentName ||
				updated.TaskName != desired.TaskName || updated.ParentTask != desired.ParentTask ||
				updated.Source != desired.Source || updated.Content != desired.Content ||
				!slices.Equal(updated.Tags, normalizeTags(desired.Tags)) {
				t.Fatalf("provenance update did not atomically demote trust: desired=%+v updated=%+v", desired, updated)
			}
			audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{
				NamespaceUID: namespaceUID, MemoryID: memory.ID, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(audits) != 2 || audits[0].Action != "memory.trust" ||
				audits[0].PreviousState != string(store.MemoryTrustTrusted) ||
				audits[0].NewState != string(store.MemoryTrustUntrusted) || audits[0].RequestID != "request-change" {
				t.Fatalf("provenance demotion audits = %+v", audits)
			}
		})
	}
}

func TestUpdateLegacyMemoryWithAuditClearsReviewedProposalProvenance(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*store.Memory)
	}{
		{name: "content", mutate: func(memory *store.Memory) { memory.Content = "edited reviewed guidance" }},
		{name: "task metadata", mutate: func(memory *store.Memory) { memory.TaskName = "different-task" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 8, 1, 7, 30, 0, 0, time.UTC)
			proposal := &store.MemoryProposal{
				Namespace: "ns-reviewed-" + strings.ReplaceAll(tt.name, " ", "-"),
				Type:      proposalTypeMemory, Title: "reviewed", Content: "reviewed guidance",
			}
			if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
				t.Fatal(err)
			}
			if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
				Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
			}); err != nil {
				t.Fatal(err)
			}
			memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
				Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "reviewer",
			})
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(memory)
			updated, err := s.UpdateLegacyMemoryWithAudit(
				ctx, memory, proposal.Namespace+"-uid", "editor", "reviewed memory changed", "request-reviewed", now,
			)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Trust != store.MemoryTrustUntrusted || updated.Source != memorySourceManual ||
				updated.SourceProposalID != "" || updated.GovernanceRevision != 2 {
				t.Fatalf("reviewed proposal provenance retained after %s change: %+v", tt.name, updated)
			}
		})
	}
}

func TestUpdateLegacyMemoryWithAuditRollsBackWhenDemotionAuditFails(t *testing.T) {
	original := governedMemoryQuotas
	t.Cleanup(func() { governedMemoryQuotas = original })
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 7, 45, 0, 0, time.UTC)
	memory := &store.Memory{Namespace: "ns-legacy-atomic", Source: "manual", Content: "trusted guidance"}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: memory.Namespace, NamespaceUID: "ns-legacy-atomic-uid", Actor: "operator", Action: "memory.trust",
		PreviousState: string(store.MemoryTrustUntrusted), NewState: string(store.MemoryTrustTrusted),
		MemoryID: memory.ID, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired := *before
	desired.Content = "changed guidance"
	governedMemoryQuotas.NamespaceAuditRows = 1
	governedMemoryQuotas.GlobalAuditRows = 100
	_, err = s.UpdateLegacyMemoryWithAudit(
		ctx, &desired, "ns-legacy-atomic-uid", "editor", "content changed", "request-atomic", now.Add(time.Second),
	)
	if !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("UpdateLegacyMemoryWithAudit error = %v, want ErrCapacity", err)
	}
	unchanged, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Content != before.Content || !unchanged.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("legacy memory changed despite rolled-back demotion audit: before=%+v after=%+v", before, unchanged)
	}
	audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{
		NamespaceUID: "ns-legacy-atomic-uid", MemoryID: memory.ID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].NewState != string(store.MemoryTrustTrusted) {
		t.Fatalf("atomic rollback audits = %+v", audits)
	}
}

func TestSetLegacyMemoryTrustWithAuditBindsReviewedSnapshot(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 30, 0, 0, time.UTC)
	namespaceUID := "ns-legacy-trust-cas-uid"
	memory := &store.Memory{Namespace: "ns-legacy-trust-cas", Source: "manual", Content: "reviewed version"}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	reviewed, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewed.GovernanceRevision = 1

	replacement := *reviewed
	replacement.Content = "replacement version"
	updated, err := s.UpdateLegacyMemoryWithAudit(
		ctx, &replacement, namespaceUID, "editor", "content changed", "request-update", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLegacyMemoryTrustWithAudit(
		ctx, reviewed, namespaceUID, store.MemoryTrustTrusted, "reviewer", "review stale content", "request-stale", now.Add(time.Second),
	); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale SetLegacyMemoryTrustWithAudit() error = %v, want ErrConflict", err)
	}
	audits, err := s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: namespaceUID, MemoryID: memory.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 0 {
		t.Fatalf("stale trust promotion wrote audits: %+v", audits)
	}

	trusted, err := s.SetLegacyMemoryTrustWithAudit(
		ctx, updated, namespaceUID, store.MemoryTrustTrusted, "reviewer", "review replacement", "request-trusted", now.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Content != updated.Content || trusted.Trust != store.MemoryTrustTrusted || trusted.GovernanceRevision != 2 {
		t.Fatalf("trusted legacy memory = %+v", trusted)
	}
	if _, err := s.SetLegacyMemoryTrustWithAudit(
		ctx, updated, namespaceUID, store.MemoryTrustReviewed, "reviewer", "stale governance revision", "request-stale-revision", now.Add(3*time.Second),
	); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale governance SetLegacyMemoryTrustWithAudit() error = %v, want ErrConflict", err)
	}
	audits, err = s.ListMemoryAudit(ctx, store.MemoryAuditFilter{NamespaceUID: namespaceUID, MemoryID: memory.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(updated.Content))
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if len(audits) != 1 || audits[0].PreviousState != string(store.MemoryTrustUntrusted) ||
		audits[0].NewState != string(store.MemoryTrustTrusted) || audits[0].ContentDigest != wantDigest {
		t.Fatalf("atomic trust audits = %+v, want content-bound transition", audits)
	}
}

func TestMemoryStoreTrustFilterRequiresAppliedProposalProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	untrusted := &store.Memory{
		Namespace: "ns-trust", Source: "manual", Content: "direct memory", Trust: store.MemoryTrustReviewed,
	}
	if err := s.CreateMemory(ctx, untrusted); err != nil {
		t.Fatalf("CreateMemory untrusted: %v", err)
	}
	if untrusted.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("direct memory trust = %q, want untrusted", untrusted.Trust)
	}
	if err := s.CreateMemory(ctx, &store.Memory{
		Namespace: "ns-trust", Source: memorySourceProposal, SourceProposalID: "proposal-spoof", Content: "spoofed",
	}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("CreateMemory spoofed proposal provenance error = %v, want ErrValidation", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memories
		(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
		"mem-spoofed-provenance", "ns-trust", memorySourceProposal, "proposal-spoof", "spoofed legacy row"); err != nil {
		t.Fatalf("insert historical spoofed row: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memories
		(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
		"mem-spoofed-applied", "ns-trust", memorySourceProposal, "proposal-spoofed-applied", "spoofed applied row"); err != nil {
		t.Fatalf("insert historical spoofed applied memory: %v", err)
	}
	spoofedAt := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memory_proposals
		(id, namespace, type, title, content, status, reviewer, applied_memory_id, applied_by, reviewed_at, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"proposal-spoofed-applied", "ns-trust", proposalTypeMemory, "spoofed applied proposal",
		"spoofed applied row", proposalStatusApplied, "forged-reviewer", "mem-spoofed-applied", "forged-applier",
		spoofedAt, spoofedAt); err != nil {
		t.Fatalf("insert historical spoofed applied proposal: %v", err)
	}

	proposal := &store.MemoryProposal{
		Namespace: "ns-trust", Type: proposalTypeMemory, Title: "reviewed", Content: "reviewed memory",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}
	untrusted.Source = memorySourceProposal
	untrusted.SourceProposalID = proposal.ID
	untrusted.Content = proposal.Content
	if err := s.UpdateMemory(ctx, untrusted); err != nil {
		t.Fatalf("UpdateMemory with spoofed provenance: %v", err)
	}
	unchanged, err := s.GetMemory(ctx, untrusted.Namespace, untrusted.ID)
	if err != nil {
		t.Fatalf("GetMemory after spoofed update: %v", err)
	}
	if unchanged.Source != "manual" || unchanged.SourceProposalID != "" || unchanged.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("UpdateMemory changed server-owned provenance: %+v", unchanged)
	}
	reviewed, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "reviewer",
	})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal: %v", err)
	}
	if reviewed.Trust != store.MemoryTrustReviewed {
		t.Fatalf("applied proposal trust = %q, want reviewed", reviewed.Trust)
	}

	all, err := s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-trust"})
	if err != nil || len(all) != 4 {
		t.Fatalf("unfiltered legacy list = %+v, %v", all, err)
	}
	reviewedOnly, err := s.ListMemories(ctx, store.MemoryFilter{
		Namespace: "ns-trust", Trust: []store.MemoryTrust{store.MemoryTrustReviewed},
	})
	if err != nil || len(reviewedOnly) != 1 || reviewedOnly[0].ID != reviewed.ID {
		t.Fatalf("reviewed trust filter = %+v, %v", reviewedOnly, err)
	}
	untrustedOnly, err := s.ListMemories(ctx, store.MemoryFilter{
		Namespace: "ns-trust", Trust: []store.MemoryTrust{store.MemoryTrustUntrusted},
	})
	if err != nil || len(untrustedOnly) != 3 {
		t.Fatalf("untrusted trust filter = %+v, %v", untrustedOnly, err)
	}
	untrustedIDs := map[string]bool{}
	for _, memory := range untrustedOnly {
		untrustedIDs[memory.ID] = true
	}
	if !untrustedIDs[untrusted.ID] || !untrustedIDs["mem-spoofed-provenance"] || !untrustedIDs["mem-spoofed-applied"] {
		t.Fatalf("untrusted trust filter ids = %+v", untrustedIDs)
	}
	trustedOnly, err := s.ListMemories(ctx, store.MemoryFilter{
		Namespace: "ns-trust", Trust: []store.MemoryTrust{store.MemoryTrustTrusted},
	})
	if err != nil || len(trustedOnly) != 0 {
		t.Fatalf("trusted trust filter = %+v, %v", trustedOnly, err)
	}
}

func TestReviewedProposalMemoryContentCannotBeEdited(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	proposal := &store.MemoryProposal{
		Namespace: "ns-reviewed-immutable",
		Type:      proposalTypeMemory,
		Title:     "Reviewed guidance",
		Content:   "Keep reviewed guidance immutable until it is reproposed.",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}
	memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "reviewer",
	})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal: %v", err)
	}

	memory.TaskName = "updated-task-metadata"
	if err := s.UpdateMemory(ctx, memory); err != nil {
		t.Fatalf("UpdateMemory metadata-only: %v", err)
	}
	memory.Content = "silently edited reviewed guidance"
	if err := s.UpdateMemory(ctx, memory); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("UpdateMemory reviewed content error = %v, want ErrConflict", err)
	}
	unchanged, err := s.GetMemory(ctx, proposal.Namespace, memory.ID)
	if err != nil {
		t.Fatalf("GetMemory after rejected edit: %v", err)
	}
	if unchanged.Content != proposal.Content || unchanged.Trust != store.MemoryTrustReviewed {
		t.Fatalf("reviewed memory changed after rejected edit: %+v", unchanged)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET content = ? WHERE namespace = ? AND id = ?`,
		"historical out-of-band edit", proposal.Namespace, memory.ID); err != nil {
		t.Fatalf("simulate historical reviewed-memory edit: %v", err)
	}
	demoted, err := s.GetMemory(ctx, proposal.Namespace, memory.ID)
	if err != nil {
		t.Fatalf("GetMemory after historical edit: %v", err)
	}
	if demoted.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("historically edited memory trust = %q, want untrusted", demoted.Trust)
	}
}

func TestMemoryProposalStore(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	proposal := &store.MemoryProposal{
		Namespace:   "ns-prop",
		TaskName:    memoryTestTaskA,
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

func TestCreateMemoryProposalClearsServerOwnedState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	proposal := &store.MemoryProposal{
		Namespace:                  "ns-proposal-state",
		Type:                       proposalTypeMemory,
		Title:                      "Cannot fabricate proposal state",
		Content:                    "Proposal creation always begins pending.",
		Status:                     proposalStatusApplied,
		Reviewer:                   "forged-reviewer",
		ReviewNote:                 "forged-review",
		AppliedMemoryID:            "mem-forged",
		ApplyOperationID:           "mop-forged",
		AppliedBy:                  "forged-applier",
		ApplicationAbandonedBy:     "forged-abandoner",
		ApplicationAbandonedReason: "forged-reason",
		ReviewedAt:                 &now,
		AppliedAt:                  &now,
		ApplicationAbandonedAt:     &now,
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}

	assertPendingProposalState := func(label string, got *store.MemoryProposal) {
		t.Helper()
		if got.Status != proposalStatusPending || got.Reviewer != "" || got.ReviewNote != "" ||
			got.AppliedMemoryID != "" || got.ApplyOperationID != "" || got.AppliedBy != "" ||
			got.ApplicationAbandonedBy != "" || got.ApplicationAbandonedReason != "" ||
			got.ReviewedAt != nil || got.AppliedAt != nil || got.ApplicationAbandonedAt != nil {
			t.Fatalf("%s proposal retained caller-owned state: %+v", label, got)
		}
	}
	assertPendingProposalState("returned", proposal)
	persisted, err := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	assertPendingProposalState("persisted", persisted)
}

func setupDiskStorePair(t *testing.T) (*Store, *Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	openStore := func(label string) *Store {
		t.Helper()
		db, err := NewDB(dbPath)
		if err != nil {
			t.Fatalf("NewDB %s: %v", label, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return NewStore(db, dbPath)
	}
	return openStore("first"), openStore("second")
}

func TestApplyMemoryProposal(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	proposal := &store.MemoryProposal{
		Namespace:   "ns-apply",
		TaskName:    memoryTestTaskA,
		AgentName:   "agent-a",
		Type:        "memory",
		Title:       "Prefer explicit migrations",
		Description: "Store migration guidance.\n\nTags: Storage, sqlite, storage",
		Content:     "Keep SQLite memory migrations idempotent and covered by tests.",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: "ns-apply",
		ID:        proposal.ID,
		Status:    "accepted",
		Reviewer:  "maintainer",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}

	memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "ns-apply",
		ID:        proposal.ID,
		AppliedBy: "coordinator",
	})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal: %v", err)
	}
	if memory.ID == "" || memory.Source != "memory_proposal" || memory.SourceProposalID != proposal.ID {
		t.Fatalf("unexpected applied memory provenance: %+v", memory)
	}
	if memory.Content != proposal.Content || memory.Namespace != "ns-apply" || memory.TaskName != memoryTestTaskA || memory.AgentName != "agent-a" {
		t.Fatalf("unexpected applied memory: %+v", memory)
	}
	if got, want := strings.Join(memory.Tags, ","), "storage,sqlite"; got != want {
		t.Fatalf("tags = %q, want %q", got, want)
	}

	updated, err := s.GetMemoryProposal(ctx, "ns-apply", proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	if updated.Status != proposalStatusApplied || updated.AppliedMemoryID != memory.ID ||
		!strings.HasPrefix(updated.ApplyOperationID, legacyProposalApplyOperationPrefix) ||
		updated.AppliedBy != "coordinator" || updated.AppliedAt == nil {
		t.Fatalf("proposal apply metadata not persisted: %+v", updated)
	}

	again, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: "ns-apply", ID: proposal.ID, AppliedBy: "other"})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal second call: %v", err)
	}
	if again.ID != memory.ID {
		t.Fatalf("second apply memory id = %q, want %q", again.ID, memory.ID)
	}
	listed, err := s.ListMemories(ctx, store.MemoryFilter{Namespace: "ns-apply", Source: "memory_proposal"})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != memory.ID {
		t.Fatalf("expected exactly one applied memory, got %+v", listed)
	}
	if err := s.ArchiveMemoryProposal(ctx, "ns-apply", proposal.ID); err == nil {
		t.Fatalf("ArchiveMemoryProposal applied proposal succeeded, want error")
	}
}

func TestApplyMemoryProposalRecoveryRequiresCanonicalExistingProvenance(t *testing.T) {
	t.Run("canonical existing row", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		proposal := &store.MemoryProposal{
			Namespace: "ns-apply-existing-canonical",
			Type:      proposalTypeMemory,
			Title:     "Canonical recovery",
			Content:   "Recover only canonical proposal provenance.",
		}
		if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
			t.Fatalf("CreateMemoryProposal: %v", err)
		}
		if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
			Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
		}); err != nil {
			t.Fatalf("ReviewMemoryProposal: %v", err)
		}
		const memoryID = "mem-existing-canonical"
		if _, err := s.db.ExecContext(ctx, `INSERT INTO memories
			(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
			memoryID, proposal.Namespace, memorySourceProposal, proposal.ID, proposal.Content); err != nil {
			t.Fatalf("insert canonical recovery row: %v", err)
		}

		memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
			Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "coordinator",
		})
		if err != nil {
			t.Fatalf("ApplyMemoryProposal: %v", err)
		}
		if memory.ID != memoryID || memory.Source != memorySourceProposal ||
			memory.SourceProposalID != proposal.ID || memory.Trust != store.MemoryTrustReviewed {
			t.Fatalf("recovered memory = %+v", memory)
		}
	})

	t.Run("non-proposal existing row", func(t *testing.T) {
		s := setupTestStore(t)
		ctx := context.Background()
		proposal := &store.MemoryProposal{
			Namespace: "ns-apply-existing-noncanonical",
			Type:      proposalTypeMemory,
			Title:     "Reject noncanonical recovery",
			Content:   "Matching content is insufficient without canonical provenance.",
		}
		if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
			t.Fatalf("CreateMemoryProposal: %v", err)
		}
		if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
			Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
		}); err != nil {
			t.Fatalf("ReviewMemoryProposal: %v", err)
		}
		const memoryID = "mem-existing-noncanonical"
		if _, err := s.db.ExecContext(ctx, `INSERT INTO memories
			(id, namespace, source, source_proposal_id, content) VALUES (?, ?, ?, ?, ?)`,
			memoryID, proposal.Namespace, "manual", proposal.ID, proposal.Content); err != nil {
			t.Fatalf("insert noncanonical recovery row: %v", err)
		}

		memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
			Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "coordinator",
		})
		if !errors.Is(err, store.ErrConflict) || memory != nil {
			t.Fatalf("ApplyMemoryProposal = (%+v, %v), want provenance conflict", memory, err)
		}
		unchangedProposal, getErr := s.GetMemoryProposal(ctx, proposal.Namespace, proposal.ID)
		if getErr != nil {
			t.Fatalf("GetMemoryProposal: %v", getErr)
		}
		if unchangedProposal.Status != proposalStatusAccepted || unchangedProposal.AppliedMemoryID != "" {
			t.Fatalf("proposal changed after provenance conflict: %+v", unchangedProposal)
		}
		unchangedMemory, getErr := s.GetMemory(ctx, proposal.Namespace, memoryID)
		if getErr != nil {
			t.Fatalf("GetMemory: %v", getErr)
		}
		if unchangedMemory.Source != "manual" || unchangedMemory.Trust != store.MemoryTrustUntrusted {
			t.Fatalf("noncanonical memory was promoted: %+v", unchangedMemory)
		}
	})
}

func TestApplyMemoryProposalRecoveryRejectsTamperedAppliedProvenance(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	proposal := &store.MemoryProposal{
		Namespace: "ns-apply-tampered",
		Type:      proposalTypeMemory,
		Title:     "Tampered applied provenance",
		Content:   "Applied recovery must revalidate provenance.",
	}
	if err := s.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: proposal.Namespace, ID: proposal.ID, Status: proposalStatusAccepted, Reviewer: "reviewer",
	}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}
	memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "coordinator",
	})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET source = 'manual' WHERE namespace = ? AND id = ?`,
		proposal.Namespace, memory.ID); err != nil {
		t.Fatalf("tamper memory provenance: %v", err)
	}

	recovered, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: proposal.Namespace, ID: proposal.ID, AppliedBy: "retry",
	})
	if !errors.Is(err, store.ErrConflict) || recovered != nil {
		t.Fatalf("ApplyMemoryProposal = (%+v, %v), want tampered-provenance conflict", recovered, err)
	}
	stored, err := s.GetMemory(ctx, proposal.Namespace, memory.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if stored.Trust != store.MemoryTrustUntrusted {
		t.Fatalf("tampered memory trust = %q, want untrusted", stored.Trust)
	}
}

func TestTagsFromProposalDescriptionUsesFirstTagsLine(t *testing.T) {
	got := tagsFromProposalDescription("Intro\nTags: Alpha, beta, alpha\nMore\nTags: ignored")
	if strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("tags = %q, want alpha,beta", strings.Join(got, ","))
	}
}

func TestIsSQLiteRetryableErrorUsesStructuredSQLiteCodesFirst(t *testing.T) {
	err := sqliteConstraintError(t)
	code, ok := sqliteErrorCode(err)
	if !ok {
		t.Fatalf("constraint error did not expose a structured sqlite code: %v", err)
	}
	if primarySQLiteCode(code) != sqlite3.SQLITE_CONSTRAINT {
		t.Fatalf("sqlite code = %d, want SQLITE_CONSTRAINT", primarySQLiteCode(code))
	}

	wrapped := fmt.Errorf("database is locked: %w", err)
	if isSQLiteRetryableError(wrapped) {
		t.Fatalf("structured non-retryable sqlite errors should not use substring fallback")
	}
	if !isSQLiteRetryableError(errors.New("database is locked")) {
		t.Fatalf("unstructured locked errors should use substring fallback")
	}
}

func sqliteConstraintError(t *testing.T) error {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec("CREATE TABLE retry_code_test (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO retry_code_test (id) VALUES (1)"); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	_, err = db.Exec("INSERT INTO retry_code_test (id) VALUES (1)")
	if err == nil {
		t.Fatalf("duplicate primary key insert succeeded, want constraint error")
	}
	return err
}

func TestApplyMemoryProposalConcurrentIdempotent(t *testing.T) {
	s1, s2 := setupDiskStorePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const ns = "ns-apply-concurrent"
	proposal := &store.MemoryProposal{
		Namespace: ns,
		TaskName:  "task-concurrent",
		Type:      "memory",
		Title:     "Concurrent proposal",
		Content:   "Only one durable memory should be created for concurrent applies.",
	}
	if err := s1.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s1.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: ns, ID: proposal.ID, Status: "accepted"}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	installHook := func(s *Store) {
		var once sync.Once
		s.applyMemoryProposalAfterAcceptedRead = func() {
			once.Do(func() {
				select {
				case ready <- struct{}{}:
				case <-ctx.Done():
					return
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}
	installHook(s1)
	installHook(s2)

	type applyResult struct {
		name   string
		memory *store.Memory
		err    error
	}
	results := make(chan applyResult, 2)
	var wg sync.WaitGroup
	for name, s := range map[string]*Store{"first": s1, "second": s2} {
		wg.Go(func() {
			memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: ns, ID: proposal.ID, AppliedBy: name})
			results <- applyResult{name: name, memory: memory, err: err}
		})
	}
	for i := range 2 {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for apply %d to read accepted proposal", i+1)
		}
	}
	releaseAll()
	wg.Wait()
	close(results)

	var memoryID string
	for result := range results {
		if result.err != nil {
			t.Fatalf("%s ApplyMemoryProposal: %v", result.name, result.err)
		}
		if result.memory == nil || result.memory.ID == "" {
			t.Fatalf("%s returned empty memory: %+v", result.name, result.memory)
		}
		if memoryID == "" {
			memoryID = result.memory.ID
		} else if result.memory.ID != memoryID {
			t.Fatalf("concurrent applies returned different memories: %q and %q", memoryID, result.memory.ID)
		}
	}

	listed, err := s1.ListMemories(ctx, store.MemoryFilter{Namespace: ns, Source: "memory_proposal"})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != memoryID || listed[0].SourceProposalID != proposal.ID {
		t.Fatalf("expected exactly one applied memory %q for proposal %q, got %+v", memoryID, proposal.ID, listed)
	}
	updated, err := s2.GetMemoryProposal(ctx, ns, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	if updated.Status != proposalStatusApplied || updated.AppliedMemoryID != memoryID {
		t.Fatalf("proposal apply metadata = %+v, want status applied and memory %q", updated, memoryID)
	}
}

func TestApplyLegacyMemoryProposalWithAuditCapturesConcurrentReplayGovernance(t *testing.T) {
	s1, s2 := setupDiskStorePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		ns           = "ns-apply-governance-race"
		namespaceUID = "ns-apply-governance-race-uid"
	)
	proposal := &store.MemoryProposal{
		Namespace: ns, Type: "memory", Title: "Concurrent governed proposal",
		Content: "Concurrent replay must include the committed governance overlay.",
	}
	if err := s1.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := s1.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: ns, ID: proposal.ID, Status: "accepted", Reviewer: "alice",
	}); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{})
	release := make(chan struct{})
	var readyOnce, releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseApply)
	s1.applyMemoryProposalAfterAcceptedRead = func() {
		readyOnce.Do(func() {
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}

	type applyResult struct {
		memory *store.Memory
		audits []store.MemoryAuditRecord
		err    error
	}
	resultCh := make(chan applyResult, 1)
	go func() {
		memory, audits, err := s1.ApplyLegacyMemoryProposalWithAudit(ctx, store.MemoryProposalApply{
			Namespace: ns, ID: proposal.ID, AppliedBy: "first",
		}, namespaceUID)
		resultCh <- applyResult{memory: memory, audits: audits, err: err}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first apply to read accepted proposal")
	}

	applied, _, err := s2.ApplyLegacyMemoryProposalWithAudit(ctx, store.MemoryProposalApply{
		Namespace: ns, ID: proposal.ID, AppliedBy: "second",
	}, namespaceUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: ns, NamespaceUID: namespaceUID, Actor: "alice", Action: "memory.trust",
		PreviousState: string(store.MemoryTrustReviewed), NewState: string(store.MemoryTrustTrusted),
		MemoryID: applied.ID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	releaseApply()

	var result applyResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for replaying apply")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.memory == nil || result.memory.ID != applied.ID {
		t.Fatalf("replayed memory = %#v, want %q", result.memory, applied.ID)
	}
	if len(result.audits) != 1 || result.audits[0].Action != "memory.trust" ||
		result.audits[0].NewState != string(store.MemoryTrustTrusted) {
		t.Fatalf("replayed governance audits = %#v", result.audits)
	}
}

func TestApplyMemoryProposalDoesNotOverwriteConcurrentStatusChange(t *testing.T) {
	s1, s2 := setupDiskStorePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const ns = "ns-apply-stale-status"
	proposal := &store.MemoryProposal{
		Namespace: ns,
		TaskName:  "task-stale",
		Type:      "memory",
		Title:     "Stale apply proposal",
		Content:   "A stale apply must not overwrite an archived proposal status.",
	}
	if err := s1.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s1.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: ns, ID: proposal.ID, Status: "accepted"}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}

	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseApply := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseApply)
	var readyOnce sync.Once
	s1.applyMemoryProposalAfterAcceptedRead = func() {
		readyOnce.Do(func() {
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}

	type applyResult struct {
		memory *store.Memory
		err    error
	}
	resultCh := make(chan applyResult, 1)
	go func() {
		memory, err := s1.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: ns, ID: proposal.ID, AppliedBy: "stale"})
		resultCh <- applyResult{memory: memory, err: err}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for apply to read accepted proposal")
	}

	if err := s2.ArchiveMemoryProposal(ctx, ns, proposal.ID); err != nil {
		t.Fatalf("ArchiveMemoryProposal: %v", err)
	}
	releaseApply()

	var result applyResult
	select {
	case result = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for stale apply result")
	}
	if result.err == nil {
		t.Fatalf("stale ApplyMemoryProposal returned memory %+v, want error", result.memory)
	}
	lowerErr := strings.ToLower(result.err.Error())
	if strings.Contains(lowerErr, "database is locked") || strings.Contains(lowerErr, "sqlite_busy") || strings.Contains(lowerErr, "sqlite_locked") {
		t.Fatalf("stale ApplyMemoryProposal returned raw SQLite concurrency error: %v", result.err)
	}
	if !errors.Is(result.err, store.ErrConflict) && !strings.Contains(result.err.Error(), "cannot be applied") {
		t.Fatalf("stale ApplyMemoryProposal error = %v, want conflict or cannot be applied", result.err)
	}

	updated, err := s1.GetMemoryProposal(ctx, ns, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	if updated.Status != "archived" || updated.AppliedMemoryID != "" {
		t.Fatalf("proposal after stale apply = %+v, want archived with no applied memory", updated)
	}
	listed, err := s2.ListMemories(ctx, store.MemoryFilter{Namespace: ns, Source: "memory_proposal"})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("stale apply created memories: %+v", listed)
	}
}

func TestArchiveMemoryProposalDoesNotOverwriteConcurrentApply(t *testing.T) {
	s1, s2 := setupDiskStorePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const ns = "ns-archive-stale-apply"
	proposal := &store.MemoryProposal{
		Namespace: ns,
		TaskName:  "task-archive-stale",
		Type:      "memory",
		Title:     "Stale archive proposal",
		Content:   "A stale archive must not overwrite an applied proposal status.",
	}
	if err := s1.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatalf("CreateMemoryProposal: %v", err)
	}
	if err := s1.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: ns, ID: proposal.ID, Status: "accepted"}); err != nil {
		t.Fatalf("ReviewMemoryProposal: %v", err)
	}

	ready := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseArchive := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseArchive)
	var readyOnce sync.Once
	s1.archiveMemoryProposalAfterActiveRead = func() {
		readyOnce.Do(func() {
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}

	archiveCh := make(chan error, 1)
	go func() {
		archiveCh <- s1.ArchiveMemoryProposal(ctx, ns, proposal.ID)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for archive to read accepted proposal")
	}

	memory, err := s2.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: ns, ID: proposal.ID, AppliedBy: "winner"})
	if err != nil {
		t.Fatalf("ApplyMemoryProposal: %v", err)
	}
	if memory == nil || memory.ID == "" {
		t.Fatalf("ApplyMemoryProposal returned empty memory: %+v", memory)
	}
	releaseArchive()

	var archiveErr error
	select {
	case archiveErr = <-archiveCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for stale archive result")
	}
	if archiveErr == nil {
		t.Fatalf("stale ArchiveMemoryProposal succeeded, want error")
	}
	lowerErr := strings.ToLower(archiveErr.Error())
	if strings.Contains(lowerErr, "database is locked") || strings.Contains(lowerErr, "sqlite_busy") || strings.Contains(lowerErr, "sqlite_locked") {
		t.Fatalf("stale ArchiveMemoryProposal returned raw SQLite concurrency error: %v", archiveErr)
	}
	if !errors.Is(archiveErr, store.ErrConflict) && !strings.Contains(lowerErr, "changed") {
		t.Fatalf("stale ArchiveMemoryProposal error = %v, want conflict", archiveErr)
	}

	updated, err := s1.GetMemoryProposal(ctx, ns, proposal.ID)
	if err != nil {
		t.Fatalf("GetMemoryProposal: %v", err)
	}
	if updated.Status != proposalStatusApplied || updated.AppliedMemoryID != memory.ID {
		t.Fatalf("proposal after stale archive = %+v, want applied with memory %q", updated, memory.ID)
	}
	listed, err := s2.ListMemories(ctx, store.MemoryFilter{Namespace: ns, Source: "memory_proposal"})
	if err != nil {
		t.Fatalf("ListMemories: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != memory.ID || listed[0].SourceProposalID != proposal.ID {
		t.Fatalf("expected exactly one applied memory %q for proposal %q, got %+v", memory.ID, proposal.ID, listed)
	}
}

func TestApplyMemoryProposalValidation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	pending := &store.MemoryProposal{
		Namespace: "ns-apply-validation",
		Type:      "memory",
		Title:     "Pending memory",
		Content:   "Only accepted proposals should apply.",
	}
	if err := s.CreateMemoryProposal(ctx, pending); err != nil {
		t.Fatalf("CreateMemoryProposal pending: %v", err)
	}
	if _, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: pending.Namespace, ID: pending.ID}); err == nil || !strings.Contains(err.Error(), "cannot be applied") {
		t.Fatalf("ApplyMemoryProposal pending error = %v, want cannot be applied", err)
	}

	skill := &store.MemoryProposal{
		Namespace: "ns-apply-validation",
		Type:      "skill",
		Title:     "Skill proposal",
		Content:   "Skill content should not become durable memory.",
	}
	if err := s.CreateMemoryProposal(ctx, skill); err != nil {
		t.Fatalf("CreateMemoryProposal skill: %v", err)
	}
	if err := s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: skill.Namespace, ID: skill.ID, Status: "accepted"}); err != nil {
		t.Fatalf("ReviewMemoryProposal skill: %v", err)
	}
	if _, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: skill.Namespace, ID: skill.ID}); err == nil || !strings.Contains(err.Error(), "cannot be applied as memory") {
		t.Fatalf("ApplyMemoryProposal skill error = %v, want cannot be applied as memory", err)
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
	if err := s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "ns-transcript", Name: "gateway-private", SessionType: store.SessionTypeGateway,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateSession gateway-private: %v", err)
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
	if err := s.AppendMessages(ctx, "ns-transcript", "gateway-private", []store.SessionMessage{
		{Role: "user", Content: "needle from another gateway conversation must never be searchable", Timestamp: now.Add(3 * time.Second)},
	}); err != nil {
		t.Fatalf("AppendMessages gateway-private: %v", err)
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

func TestUpdateMemoryPreservesOrdinarySourceChanges(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	memory := &store.Memory{Namespace: "ns-source", Content: "content", Source: "manual"}
	if err := s.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	memory.Source = testMetadataImport
	if err := s.UpdateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	updated, err := s.GetMemory(ctx, memory.Namespace, memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Source != testMetadataImport || updated.SourceProposalID != "" {
		t.Fatalf("updated provenance = source %q proposal %q", updated.Source, updated.SourceProposalID)
	}
}
