package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	memoryTenant      = "tenant-a"
	memoryOtherTenant = "tenant-b"
)

func memoryIDs(memories []storagev1.Memory) []string {
	ids := make([]string, 0, len(memories))
	for _, memory := range memories {
		ids = append(ids, memory.ID)
	}
	return ids
}

func proposalIDs(proposals []storagev1.MemoryProposal) []string {
	ids := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		ids = append(ids, proposal.ID)
	}
	return ids
}

func runMemories(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	t.Run("memories are namespaced and listed newest first", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for _, invalid := range []*storagev1.Memory{
			nil,
			{Content: "no namespace"},
			{Namespace: memoryTenant, Content: "  "},
		} {
			require.ErrorIs(t, p.CreateMemory(ctx, invalid), storagev1.ErrValidation)
		}
		assigned := &storagev1.Memory{Namespace: memoryTenant, Content: "assigned", CreatedAt: at, UpdatedAt: at}
		require.NoError(t, p.CreateMemory(ctx, assigned))
		require.NotEmpty(t, assigned.ID, "the provider assigns an ID")
		require.Equal(t, storagev1.MemorySourceManual, assigned.Source, "the source defaults to manual")
		stored, err := p.GetMemory(ctx, memoryTenant, assigned.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemorySourceManual, stored.Source)
		// mem-new was created before mem-old but updated after it: memories
		// list by update time, not creation time.
		for _, memory := range []struct {
			id               string
			created, updated time.Duration
		}{{"mem-old", 2 * time.Minute, 2 * time.Minute}, {"mem-new", time.Minute, 3 * time.Minute}} {
			require.NoError(t, p.CreateMemory(ctx, &storagev1.Memory{ID: memory.id, Namespace: memoryTenant, Content: memory.id,
				Source: "task", Tags: []string{"ops"}, CreatedAt: at.Add(memory.created), UpdatedAt: at.Add(memory.updated)}))
		}
		require.NoError(t, p.CreateMemory(ctx, &storagev1.Memory{ID: "mem-foreign", Namespace: memoryOtherTenant,
			Content: "foreign", CreatedAt: at, UpdatedAt: at}))
		require.ErrorIs(t, p.CreateMemory(ctx, &storagev1.Memory{ID: "mem-old", Namespace: memoryTenant,
			Content: "reused", CreatedAt: at, UpdatedAt: at}), storagev1.ErrConflict, "a reused memory ID")
		require.ErrorIs(t, p.CreateMemory(ctx, &storagev1.Memory{ID: "mem-foreign", Namespace: memoryTenant,
			Content: "reused", CreatedAt: at, UpdatedAt: at}), storagev1.ErrConflict, "memory IDs are unique across namespaces")
		require.NoError(t, p.CreateMemory(ctx, &storagev1.Memory{ID: "mem-from-proposal", Namespace: memoryOtherTenant,
			Content: "from a proposal", SourceProposalID: "mprop-source", CreatedAt: at, UpdatedAt: at}))
		require.ErrorIs(t, p.CreateMemory(ctx, &storagev1.Memory{ID: "mem-from-proposal-again", Namespace: memoryOtherTenant,
			Content: "again", SourceProposalID: "mprop-source", CreatedAt: at, UpdatedAt: at}), storagev1.ErrConflict,
			"one memory per source proposal in a namespace")

		listed, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant})
		require.NoError(t, err)
		require.Equal(t, []string{"mem-new", "mem-old", assigned.ID}, memoryIDs(listed))
		listed, err = p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant, Limit: 1})
		require.NoError(t, err)
		require.Equal(t, []string{"mem-new"}, memoryIDs(listed), "the limit bounds the result")
		got, err := p.GetMemory(ctx, memoryTenant, "mem-old")
		require.NoError(t, err)
		require.Equal(t, "mem-old", got.Content)
		require.Equal(t, []string{"ops"}, got.Tags)
		foreign, err := p.GetMemory(ctx, memoryOtherTenant, "mem-foreign")
		require.NoError(t, err)
		require.Equal(t, "foreign", foreign.Content)
		reused := *foreign
		reused.SourceProposalID = "mprop-source"
		require.ErrorIs(t, p.UpdateMemory(ctx, &reused), storagev1.ErrConflict, "an update cannot reuse a source proposal")
		_, err = p.GetMemory(ctx, memoryOtherTenant, "mem-old")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a memory is read only in its namespace")
		_, err = p.ListMemories(ctx, storagev1.MemoryFilter{})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("filters, updates, disabling and soft deletion", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for _, memory := range []storagev1.Memory{
			{ID: "mem-agent", AgentName: "agent", TaskName: "task", SessionName: "session", Source: "task",
				Content: "Deploy with the blue pipeline", Tags: []string{"deploy"}},
			{ID: "mem-other", AgentName: "other", Source: "manual", Content: "Unrelated note"},
		} {
			memory.Namespace, memory.CreatedAt, memory.UpdatedAt = memoryTenant, at, at
			require.NoError(t, p.CreateMemory(ctx, &memory))
		}
		for name, filter := range map[string]storagev1.MemoryFilter{
			"agent":   {AgentName: "agent"},
			"task":    {TaskName: "task"},
			"session": {SessionName: "session"},
			"source":  {Source: "task"},
			"tag":     {Tags: []string{"DEPLOY"}},
			"query":   {Query: "blue PIPELINE"},
			"ids":     {IDs: []string{"mem-agent", "missing"}},
		} {
			filter.Namespace = memoryTenant
			listed, err := p.ListMemories(ctx, filter)
			require.NoError(t, err, name)
			require.Equal(t, []string{"mem-agent"}, memoryIDs(listed), name)
		}

		updated := &storagev1.Memory{ID: "mem-agent", Namespace: memoryTenant, Source: "task", Content: "Deploy with the green pipeline"}
		require.NoError(t, p.UpdateMemory(ctx, updated))
		got, err := p.GetMemory(ctx, memoryTenant, "mem-agent")
		require.NoError(t, err)
		require.Equal(t, "Deploy with the green pipeline", got.Content)
		require.True(t, got.UpdatedAt.After(at), "an update refreshes UpdatedAt")
		hidden := *updated
		hidden.Disabled = true
		require.NoError(t, p.UpdateMemory(ctx, &hidden))
		listed, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant})
		require.NoError(t, err)
		require.Equal(t, []string{"mem-other"}, memoryIDs(listed), "an update replaces Disabled")
		require.NoError(t, p.UpdateMemory(ctx, updated))
		require.ErrorIs(t, p.UpdateMemory(ctx, &storagev1.Memory{ID: "missing", Namespace: memoryTenant, Content: "x"}),
			storagev1.ErrNotFound)

		require.NoError(t, p.SetMemoryDisabled(ctx, memoryTenant, "mem-agent", true))
		listed, err = p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant})
		require.NoError(t, err)
		require.Equal(t, []string{"mem-other"}, memoryIDs(listed), "disabled memories are not recalled")
		listed, err = p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant, IncludeDisabled: true, IDs: []string{"mem-agent"}})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.True(t, listed[0].Disabled)

		require.NoError(t, p.DeleteMemory(ctx, memoryTenant, "mem-other"))
		listed, err = p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: memoryTenant, IncludeDisabled: true})
		require.NoError(t, err)
		require.Equal(t, []string{"mem-agent"}, memoryIDs(listed), "deleted memories need IncludeDeleted")
		deleted, err := p.GetMemory(ctx, memoryTenant, "mem-other")
		require.NoError(t, err, "deletion keeps the provenance record")
		require.True(t, deleted.Deleted)
		require.True(t, deleted.Disabled)
		require.ErrorIs(t, p.SetMemoryDisabled(ctx, memoryTenant, "mem-other", false), storagev1.ErrNotFound,
			"a deleted memory cannot be re-enabled")
		require.ErrorIs(t, p.DeleteMemory(ctx, memoryOtherTenant, "mem-agent"), storagev1.ErrNotFound)
	})

	t.Run("proposals are reviewed once and archived", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.CreateMemoryProposal(ctx, nil), storagev1.ErrValidation)
		require.ErrorIs(t, p.CreateMemoryProposal(ctx, &storagev1.MemoryProposal{Namespace: memoryTenant}), storagev1.ErrValidation,
			"a title is required")
		reviewedAt := at
		for name, outcome := range map[string]storagev1.MemoryProposal{
			"unknown status":   {Status: "merged"},
			"accepted status":  {Status: storagev1.MemoryProposalStatusAccepted},
			"applied status":   {Status: storagev1.MemoryProposalStatusApplied, AppliedMemoryID: "mem-1"},
			"reviewer":         {Reviewer: "someone"},
			"review note":      {ReviewNote: "fine"},
			"review time":      {ReviewedAt: &reviewedAt},
			"applied memory":   {AppliedMemoryID: " "},
			"applier":          {AppliedBy: "someone"},
			"application time": {AppliedAt: &reviewedAt},
		} {
			outcome.Namespace, outcome.Title = memoryTenant, "carries an outcome"
			require.ErrorIs(t, p.CreateMemoryProposal(ctx, &outcome), storagev1.ErrValidation,
				"a new proposal carries no review or apply outcome: %s", name)
		}
		listed, err := p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{Namespace: memoryTenant})
		require.NoError(t, err)
		require.Empty(t, listed, "a refused proposal is not stored")

		// The skill proposal was created first but updated last: proposals
		// list by creation time.
		skill := &storagev1.MemoryProposal{Namespace: memoryTenant, Title: "Retry skill", SkillName: "retry",
			Content: "Back off exponentially", CreatedAt: at, UpdatedAt: at.Add(time.Hour)}
		require.NoError(t, p.CreateMemoryProposal(ctx, skill))
		require.NotEmpty(t, skill.ID)
		require.Equal(t, storagev1.MemoryProposalTypeSkill, skill.Type, "the type defaults to skill")
		require.Equal(t, storagev1.MemoryProposalStatusPending, skill.Status)
		memory := &storagev1.MemoryProposal{ID: "mprop-memory", Namespace: memoryTenant, Type: " Memory ", Title: "Fact",
			Content: "The cache is warm", CreatedAt: at.Add(time.Minute), UpdatedAt: at.Add(time.Minute)}
		require.NoError(t, p.CreateMemoryProposal(ctx, memory))
		require.Equal(t, storagev1.MemoryProposalTypeMemory, memory.Type)
		require.ErrorIs(t, p.CreateMemoryProposal(ctx, &storagev1.MemoryProposal{ID: "mprop-memory", Namespace: memoryTenant,
			Title: "Reused"}), storagev1.ErrConflict, "a reused proposal ID")
		require.ErrorIs(t, p.CreateMemoryProposal(ctx, &storagev1.MemoryProposal{ID: "mprop-memory", Namespace: memoryOtherTenant,
			Title: "Reused"}), storagev1.ErrConflict, "proposal IDs are unique across namespaces")

		listed, err = p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{Namespace: memoryTenant})
		require.NoError(t, err)
		require.Equal(t, []string{"mprop-memory", skill.ID}, proposalIDs(listed), "newest first")
		listed, err = p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{Namespace: memoryTenant, Limit: 1})
		require.NoError(t, err)
		require.Equal(t, []string{"mprop-memory"}, proposalIDs(listed), "the limit bounds the result")
		listed, err = p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{Namespace: memoryTenant, Type: "skill", Query: "EXPONENTIALLY"})
		require.NoError(t, err)
		require.Equal(t, []string{skill.ID}, proposalIDs(listed))
		_, err = p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{})
		require.ErrorIs(t, err, storagev1.ErrValidation)

		require.ErrorIs(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: memoryTenant, ID: "mprop-memory", Status: "archived"}),
			storagev1.ErrValidation, "a review accepts or rejects")
		require.ErrorIs(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: memoryOtherTenant, ID: "mprop-memory", Status: "accepted"}),
			storagev1.ErrNotFound)
		require.NoError(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: memoryTenant, ID: "mprop-memory",
			Status: "accepted", Reviewer: "reviewer", ReviewNote: "looks right"}))
		reviewed, err := p.GetMemoryProposal(ctx, memoryTenant, "mprop-memory")
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusAccepted, reviewed.Status)
		require.Equal(t, []string{"reviewer", "looks right"}, []string{reviewed.Reviewer, reviewed.ReviewNote})
		require.NotNil(t, reviewed.ReviewedAt)
		require.Empty(t, reviewed.AppliedMemoryID, "review never applies a proposal")
		require.ErrorIs(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: memoryTenant, ID: "mprop-memory", Status: "rejected"}),
			storagev1.ErrValidation, "a proposal is reviewed once")

		require.NoError(t, p.ArchiveMemoryProposal(ctx, memoryTenant, skill.ID))
		require.NoError(t, p.ArchiveMemoryProposal(ctx, memoryTenant, skill.ID), "archiving is idempotent")
		archived, err := p.GetMemoryProposal(ctx, memoryTenant, skill.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusArchived, archived.Status)
		require.ErrorIs(t, p.ArchiveMemoryProposal(ctx, memoryTenant, "missing"), storagev1.ErrNotFound)
	})
}
