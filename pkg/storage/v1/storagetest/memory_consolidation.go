package storagetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The cases in this file hold providers to memory generations and to the
// consolidation proposals that pin them: merge, supersede and retire.

func consolidationMemory(t *testing.T, p storagev1.StateProvider, id string, at time.Time) {
	t.Helper()
	memory := &storagev1.Memory{ID: id, Namespace: conditionalNamespace, Content: "content of " + id, CreatedAt: at, UpdatedAt: at}
	require.NoError(t, p.CreateMemory(t.Context(), memory))
}

func requireMemoryGeneration(t *testing.T, p storagev1.StateProvider, id string, want uint64, msg string) *storagev1.Memory {
	t.Helper()
	memory, err := p.GetMemory(t.Context(), conditionalNamespace, id)
	require.NoError(t, err)
	require.Equal(t, want, memory.Generation, "%s: generation of %s", msg, id)
	return memory
}

func acceptedConsolidationProposal(t *testing.T, p storagev1.StateProvider, proposal *storagev1.MemoryProposal) *storagev1.MemoryProposal {
	t.Helper()
	proposal.Namespace = conditionalNamespace
	if proposal.Title == "" {
		proposal.Title = "Consolidate " + proposal.ID
	}
	require.NoError(t, p.CreateMemoryProposal(t.Context(), proposal))
	require.NoError(t, p.ReviewMemoryProposal(t.Context(), storagev1.MemoryProposalReview{
		Namespace: conditionalNamespace, ID: proposal.ID, Status: storagev1.MemoryProposalStatusAccepted, Reviewer: "reviewer",
	}))
	return proposal
}

// applyConsolidation resolves and commits a consolidation proposal once, as
// Orka does after authorizing it.
func applyConsolidation(ctx context.Context, p storagev1.StateProvider, proposal *storagev1.MemoryProposal, at time.Time) (storagev1.MemoryProposalApplyTarget, *storagev1.Memory, error) {
	target, err := p.ResolveMemoryProposalApplication(ctx, proposal.Namespace, proposal.ID)
	if err != nil {
		return target, nil, err
	}
	application := storagev1.MemoryProposalApplication{Target: target, AppliedBy: "applier", AppliedAt: at}
	if target.MemoryID == "" {
		application.Memory = &storagev1.Memory{Namespace: proposal.Namespace, Content: proposal.Content, CreatedAt: at, UpdatedAt: at}
	}
	memory, err := p.CommitMemoryProposalApplication(ctx, application)
	return target, memory, err
}

func runMemoryConsolidation(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	t.Run("each change adds one generation and a recall adds none", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		created := &storagev1.Memory{ID: "mem-gen", Namespace: conditionalNamespace, Content: "first", CreatedAt: at, UpdatedAt: at,
			Generation: 41, SupersededBy: "mem-forged"}
		require.NoError(t, p.CreateMemory(ctx, created))
		require.Equal(t, uint64(1), created.Generation, "creation writes generation 1 back")
		stored := requireMemoryGeneration(t, p, "mem-gen", 1, "a created memory ignores the generation it names")
		require.Empty(t, stored.SupersededBy, "a created memory is superseded by nothing")

		update := *stored
		update.Content, update.Generation = "second", 99
		require.NoError(t, p.UpdateMemory(ctx, &update))
		requireMemoryGeneration(t, p, "mem-gen", 2, "an update")
		resaved := *requireMemoryGeneration(t, p, "mem-gen", 2, "before saving it unchanged")
		require.NoError(t, p.UpdateMemory(ctx, &resaved))
		requireMemoryGeneration(t, p, "mem-gen", 2, "saving a memory unchanged keeps its generation")
		retagged := resaved
		retagged.Tags = []string{"ops"}
		require.NoError(t, p.UpdateMemory(ctx, &retagged))
		requireMemoryGeneration(t, p, "mem-gen", 3, "changing only the tags is a change")
		require.NoError(t, p.UpdateMemory(ctx, &retagged))
		requireMemoryGeneration(t, p, "mem-gen", 3, "saving the same tags again is not")
		require.NoError(t, p.SetMemoryDisabled(ctx, conditionalNamespace, "mem-gen", true))
		requireMemoryGeneration(t, p, "mem-gen", 4, "disabling")
		require.NoError(t, p.SetMemoryDisabled(ctx, conditionalNamespace, "mem-gen", true))
		requireMemoryGeneration(t, p, "mem-gen", 4, "disabling a disabled memory changes nothing")
		require.NoError(t, p.SetMemoryDisabled(ctx, conditionalNamespace, "mem-gen", false))
		requireMemoryGeneration(t, p, "mem-gen", 5, "enabling")
		require.NoError(t, p.RecordMemoryRecall(ctx, storagev1.MemoryRecall{Namespace: conditionalNamespace, IDs: []string{"mem-gen"}, RecalledAt: at}))
		requireMemoryGeneration(t, p, "mem-gen", 5, "a recall")
		require.NoError(t, p.DeleteMemory(ctx, conditionalNamespace, "mem-gen"))
		requireMemoryGeneration(t, p, "mem-gen", 6, "deleting")
		require.NoError(t, p.DeleteMemory(ctx, conditionalNamespace, "mem-gen"))
		requireMemoryGeneration(t, p, "mem-gen", 6, "deleting a deleted memory changes nothing")
	})

	t.Run("proposals name targets only for consolidation and keep them", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		one := []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}}
		two := []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 3}}
		tooMany := make([]storagev1.MemoryProposalTarget, storagev1.MaxMemoryProposalTargets+1)
		for i := range tooMany {
			tooMany[i] = storagev1.MemoryProposalTarget{MemoryID: "mem-" + string(rune('a'+i)), Generation: 1}
		}
		tooManySources := make([]storagev1.MemoryProposalSource, storagev1.MaxMemoryProposalSources+1)
		for i := range tooManySources {
			tooManySources[i] = storagev1.MemoryProposalSource{SessionName: "session"}
		}
		for name, invalid := range map[string]storagev1.MemoryProposal{
			"targets on a memory proposal": {Type: storagev1.MemoryProposalTypeMemory, Content: "x", Targets: one},
			"merge of one memory":          {Type: storagev1.MemoryProposalTypeMerge, Content: "x", Targets: one},
			"supersede of two memories":    {Type: storagev1.MemoryProposalTypeSupersede, Content: "x", Targets: two},
			"retire of nothing":            {Type: storagev1.MemoryProposalTypeRetire},
			"merge without content":        {Type: storagev1.MemoryProposalTypeMerge, Content: " ", Targets: two},
			"supersede without content":    {Type: storagev1.MemoryProposalTypeSupersede, Targets: one},
			"a target without an ID":       {Type: storagev1.MemoryProposalTypeRetire, Targets: []storagev1.MemoryProposalTarget{{MemoryID: " ", Generation: 1}}},
			"a target without a generation": {Type: storagev1.MemoryProposalTypeRetire,
				Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-a"}}},
			"a target named twice": {Type: storagev1.MemoryProposalTypeMerge, Content: "x",
				Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-a", Generation: 2}}},
			"too many targets": {Type: storagev1.MemoryProposalTypeRetire, Targets: tooMany},
			"a source without a Session": {Type: storagev1.MemoryProposalTypeMemory, Content: "x",
				Sources: []storagev1.MemoryProposalSource{{MessageID: "message-1"}}},
			"too many sources": {Type: storagev1.MemoryProposalTypeMemory, Content: "x", Sources: tooManySources},
		} {
			invalid.Namespace, invalid.Title = conditionalNamespace, name
			require.ErrorIs(t, p.CreateMemoryProposal(ctx, &invalid), storagev1.ErrValidation, name)
		}

		merge := &storagev1.MemoryProposal{ID: "mprop-merge-kept", Namespace: conditionalNamespace, Type: " MEMORY_MERGE ",
			Title: "Merge", Content: "merged", Targets: two,
			Sources: []storagev1.MemoryProposalSource{{SessionName: "session-a", MessageID: "task:uid:assistant"}, {SessionName: "session-b"}}}
		require.NoError(t, p.CreateMemoryProposal(ctx, merge))
		require.Equal(t, storagev1.MemoryProposalTypeMerge, merge.Type)
		stored, err := p.GetMemoryProposal(ctx, conditionalNamespace, merge.ID)
		require.NoError(t, err)
		require.Equal(t, two, stored.Targets)
		require.Equal(t, merge.Sources, stored.Sources)
		listed, err := p.ListMemoryProposals(ctx, storagev1.MemoryProposalFilter{Namespace: conditionalNamespace, Type: storagev1.MemoryProposalTypeMerge})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, two, listed[0].Targets)
		plain := &storagev1.MemoryProposal{ID: "mprop-plain", Namespace: conditionalNamespace, Type: storagev1.MemoryProposalTypeMemory,
			Title: "Plain", Content: "plain"}
		require.NoError(t, p.CreateMemoryProposal(ctx, plain))
		storedPlain, err := p.GetMemoryProposal(ctx, conditionalNamespace, plain.ID)
		require.NoError(t, err)
		require.Empty(t, storedPlain.Targets)
		require.Empty(t, storedPlain.Sources)
	})

	t.Run("a merge creates one memory and retires every target into it", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		consolidationMemory(t, p, "mem-a", at)
		consolidationMemory(t, p, "mem-b", at)
		require.NoError(t, p.SetMemoryDisabled(ctx, conditionalNamespace, "mem-b", true))
		proposal := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-merge", Type: storagev1.MemoryProposalTypeMerge,
			Content: "merged content", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 2}}})
		applied := at.Add(time.Hour)
		target, memory, err := applyConsolidation(ctx, p, proposal, applied)
		require.NoError(t, err)
		require.Empty(t, target.MemoryID, "a merge resolves to the memory it creates")
		require.Equal(t, "merged content", memory.Content)
		require.Equal(t, uint64(1), memory.Generation)
		require.Equal(t, proposal.ID, memory.SourceProposalID)
		for id, generation := range map[string]uint64{"mem-a": 2, "mem-b": 3} {
			retired := requireMemoryGeneration(t, p, id, generation, "a merged target")
			require.True(t, retired.Deleted, id)
			require.True(t, retired.Disabled, id)
			require.Equal(t, memory.ID, retired.SupersededBy, id)
			requireSameInstant(t, applied, retired.UpdatedAt, "a merged target's update time")
		}
		recorded, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusApplied, recorded.Status)
		require.Equal(t, memory.ID, recorded.AppliedMemoryID)

		_, again, err := applyConsolidation(ctx, p, proposal, applied.Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, memory.ID, again.ID, "an applied merge resolves to its memory")
		requireMemoryGeneration(t, p, "mem-a", 2, "replaying an applied merge")
	})

	t.Run("a memory that names a merge as its source is never adopted", func(t *testing.T) {
		for name, linkedID := range map[string]string{"a separate memory": "mem-linked", "a target as the memory": "mem-a"} {
			t.Run(name, func(t *testing.T) {
				p, ctx := newProvider(t), t.Context()
				const proposalID = "mprop-linked-merge"
				// Anyone who can create a memory can name a proposal as its source.
				require.NoError(t, p.CreateMemory(ctx, &storagev1.Memory{ID: linkedID, Namespace: conditionalNamespace,
					Source: storagev1.MemorySourceProposal, SourceProposalID: proposalID, Content: "content nobody reviewed",
					CreatedAt: at, UpdatedAt: at}))
				if linkedID != "mem-a" {
					consolidationMemory(t, p, "mem-a", at)
				}
				consolidationMemory(t, p, "mem-b", at)
				proposal := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: proposalID, Type: storagev1.MemoryProposalTypeMerge,
					Content: "merged", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 1}}})
				before := observeMemoriesIn(t, p)
				target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
				require.NoError(t, err)
				require.Equal(t, linkedID, target.MemoryID)
				memory, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedBy: "applier", AppliedAt: at})
				require.ErrorIs(t, err, storagev1.ErrConflict,
					"a consolidation proposal creates its memory as it applies, so a memory that already names it was not reviewed")
				require.NotErrorIs(t, err, storagev1.ErrPreconditionFailed)
				require.Nil(t, memory)
				require.Equal(t, before, observeMemoriesIn(t, p), "no target is retired")
				recorded, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
				require.NoError(t, err)
				require.Empty(t, recorded.AppliedMemoryID)
			})
		}
	})

	t.Run("a supersede replaces one memory", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		consolidationMemory(t, p, "mem-old", at)
		proposal := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-supersede", Type: storagev1.MemoryProposalTypeSupersede,
			Content: "current value", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-old", Generation: 1}}})
		_, memory, err := applyConsolidation(ctx, p, proposal, at.Add(time.Hour))
		require.NoError(t, err)
		retired := requireMemoryGeneration(t, p, "mem-old", 2, "a superseded target")
		require.True(t, retired.Deleted)
		require.Equal(t, memory.ID, retired.SupersededBy)
		listed, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: conditionalNamespace})
		require.NoError(t, err)
		require.Equal(t, []string{memory.ID}, memoryIDs(listed), "only the new memory is recalled")
	})

	t.Run("a retire disables every target and returns the first", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		consolidationMemory(t, p, "mem-first", at)
		consolidationMemory(t, p, "mem-second", at)
		proposal := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-retire", Type: storagev1.MemoryProposalTypeRetire,
			Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-first", Generation: 1}, {MemoryID: "mem-second", Generation: 1}}})
		target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, "mem-first", target.MemoryID, "a retire resolves to its first target")
		_, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedAt: at,
			Memory: &storagev1.Memory{Namespace: conditionalNamespace, Content: "x"}})
		require.ErrorIs(t, err, storagev1.ErrValidation, "a retire creates no memory")
		memory, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedBy: "applier", AppliedAt: at})
		require.NoError(t, err)
		require.Equal(t, "mem-first", memory.ID)
		require.True(t, memory.Disabled)
		for _, id := range []string{"mem-first", "mem-second"} {
			retired := requireMemoryGeneration(t, p, id, 2, "a retired target")
			require.True(t, retired.Disabled, id)
			require.False(t, retired.Deleted, "a retire only disables, so a person can enable it again")
			require.Empty(t, retired.SupersededBy)
		}
		recorded, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusApplied, recorded.Status)
		require.Equal(t, "mem-first", recorded.AppliedMemoryID)
		replay, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		again, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: replay, AppliedAt: at})
		require.NoError(t, err)
		require.Equal(t, "mem-first", again.ID)
		requireMemoryGeneration(t, p, "mem-second", 2, "replaying an applied retire")
	})

	t.Run("retiring a disabled memory keeps its generation", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		consolidationMemory(t, p, "mem-off", at)
		consolidationMemory(t, p, "mem-on", at)
		require.NoError(t, p.SetMemoryDisabled(ctx, conditionalNamespace, "mem-off", true))
		disabled := requireMemoryGeneration(t, p, "mem-off", 2, "a disabled memory")
		retire := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-retire-off", Type: storagev1.MemoryProposalTypeRetire,
			Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-off", Generation: 2}, {MemoryID: "mem-on", Generation: 1}}})
		_, _, err := applyConsolidation(ctx, p, retire, at.Add(time.Hour))
		require.NoError(t, err)
		unchanged := requireMemoryGeneration(t, p, "mem-off", 2, "retiring a disabled memory changes nothing")
		require.True(t, disabled.UpdatedAt.Equal(unchanged.UpdatedAt))
		requireMemoryGeneration(t, p, "mem-on", 2, "retiring an enabled memory changes it")

		merge := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-after-retire", Type: storagev1.MemoryProposalTypeMerge,
			Content: "merged", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-off", Generation: 2}, {MemoryID: "mem-on", Generation: 2}}})
		_, _, err = applyConsolidation(ctx, p, merge, at.Add(2*time.Hour))
		require.NoError(t, err, "a proposal that read the disabled memory stays current")
	})

	t.Run("a stale proposal changes nothing", func(t *testing.T) {
		for name, change := range map[string]func(t *testing.T, p storagev1.StateProvider){
			"a target was updated": func(t *testing.T, p storagev1.StateProvider) {
				updated := *requireMemoryGeneration(t, p, "mem-b", 1, "before")
				updated.Content = "edited by a person"
				require.NoError(t, p.UpdateMemory(t.Context(), &updated))
			},
			"a target was deleted": func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.DeleteMemory(t.Context(), conditionalNamespace, "mem-b"))
			},
		} {
			for _, proposalType := range []string{storagev1.MemoryProposalTypeMerge, storagev1.MemoryProposalTypeRetire} {
				t.Run(name+" before a "+proposalType, func(t *testing.T) {
					p, ctx := newProvider(t), t.Context()
					consolidationMemory(t, p, "mem-a", at)
					consolidationMemory(t, p, "mem-b", at)
					proposal := acceptedConsolidationProposal(t, p, &storagev1.MemoryProposal{ID: "mprop-stale", Type: proposalType,
						Content: "merged", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 1}}})
					change(t, p)
					before := observeMemoriesIn(t, p)

					_, memory, err := applyConsolidation(ctx, p, proposal, at.Add(time.Hour))
					require.ErrorIs(t, err, storagev1.ErrConflict)
					require.NotErrorIs(t, err, storagev1.ErrPreconditionFailed, "a stale proposal stays stale; resolving again cannot help")
					require.Nil(t, memory)
					require.Equal(t, before, observeMemoriesIn(t, p), "no memory is created or changed")
					recorded, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
					require.NoError(t, err)
					require.Equal(t, storagev1.MemoryProposalStatusAccepted, recorded.Status, "the proposal stays accepted for a person to archive")
					require.Empty(t, recorded.AppliedMemoryID)
				})
			}
		}

		p, ctx := newProvider(t), t.Context()
		consolidationMemory(t, p, "mem-elsewhere", at)
		proposal := &storagev1.MemoryProposal{ID: "mprop-foreign", Namespace: "other-namespace", Type: storagev1.MemoryProposalTypeRetire,
			Title: "Retire", Targets: []storagev1.MemoryProposalTarget{{MemoryID: "mem-elsewhere", Generation: 1}}}
		require.NoError(t, p.CreateMemoryProposal(ctx, proposal))
		require.NoError(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: "other-namespace", ID: proposal.ID,
			Status: storagev1.MemoryProposalStatusAccepted}))
		_, _, err := applyConsolidation(ctx, p, proposal, at)
		require.ErrorIs(t, err, storagev1.ErrConflict, "a proposal changes memories only in its namespace")
		requireMemoryGeneration(t, p, "mem-elsewhere", 1, "a memory in another namespace")
	})
}

func observeMemoriesIn(t *testing.T, p storagev1.StateProvider) []storagev1.Memory {
	t.Helper()
	memories, err := p.ListMemories(t.Context(), storagev1.MemoryFilter{Namespace: conditionalNamespace, IncludeDisabled: true, IncludeDeleted: true})
	require.NoError(t, err)
	return memories
}
