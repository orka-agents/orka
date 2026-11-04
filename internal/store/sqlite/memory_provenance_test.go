package sqlite

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// Apply writes its memory and marks the proposal applied in one transaction.
// A memory that already names an accepted, unapplied proposal was written some
// other way, so apply refuses it without reading it or changing the proposal.
func TestApplyMemoryProposalRefusesMemoryItDidNotWrite(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	const ns = "ns-apply-foreign"

	proposal := &store.MemoryProposal{Namespace: ns, Type: "memory", Title: "Reviewed", Content: "Reviewed content."}
	require.NoError(t, s.CreateMemoryProposal(ctx, proposal))
	require.NoError(t, s.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: ns, ID: proposal.ID, Status: "accepted", Reviewer: "reviewer"}))
	foreign := &store.Memory{Namespace: ns, Source: memorySourceProposal, SourceProposalID: proposal.ID, Content: "Unreviewed content."}
	require.NoError(t, s.CreateMemory(ctx, foreign))

	for range 2 {
		memory, err := s.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
			Namespace: ns, ID: proposal.ID, AppliedBy: "applier",
			AuthorizeExistingMemory: func(context.Context, string, string) error {
				t.Fatal("a refused memory is never read")
				return nil
			},
		})
		require.ErrorIs(t, err, store.ErrConflict)
		require.NotContains(t, err.Error(), foreign.ID)
		require.Nil(t, memory)
	}

	stored, err := s.GetMemoryProposal(ctx, ns, proposal.ID)
	require.NoError(t, err)
	require.Equal(t, proposalStatusAccepted, stored.Status)
	require.Empty(t, stored.AppliedMemoryID)
	require.Empty(t, stored.AppliedBy)
	require.Nil(t, stored.AppliedAt)

	memories, err := s.ListMemories(ctx, store.MemoryFilter{Namespace: ns, IncludeDisabled: true, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, memories, 1)
	require.Equal(t, foreign.ID, memories[0].ID)
	require.Equal(t, "Unreviewed content.", memories[0].Content)
}
