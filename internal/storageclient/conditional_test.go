package storageclient_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// protocolStore is the SQLite provider with hooks on the conditional
// commands' provider calls.
type protocolStore struct {
	*sqlite.Store
	afterResolve func()
	previews     atomic.Int32
	commits      atomic.Int32
}

func (s *protocolStore) ResolveMemoryProposalApplication(ctx context.Context, namespace, id string) (store.MemoryProposalApplyTarget, error) {
	target, err := s.Store.ResolveMemoryProposalApplication(ctx, namespace, id)
	if hook := s.afterResolve; hook != nil {
		s.afterResolve = nil
		hook()
	}
	return target, err
}

func (s *protocolStore) PreviewScanTaskIngestion(ctx context.Context, preview store.ScanIngestionPreview) ([]store.ScanIngestionReadResult, error) {
	s.previews.Add(1)
	return s.Store.PreviewScanTaskIngestion(ctx, preview)
}

func (s *protocolStore) CommitScanTaskIngestion(ctx context.Context, commit store.ScanTaskIngestionCommit) (bool, error) {
	s.commits.Add(1)
	return s.Store.CommitScanTaskIngestion(ctx, commit)
}

func newProtocolClient(t *testing.T) (*storageclient.Client, *protocolStore) {
	t.Helper()
	backend := &protocolStore{Store: openSQLite(t)}
	return storageclienttest.NewEmbedded(t, backend), backend
}

func acceptedProposal(t *testing.T, client *storageclient.Client, id, description string) *store.MemoryProposal {
	t.Helper()
	proposal := &store.MemoryProposal{ID: id, Namespace: "tenant", Type: store.MemoryProposalTypeMemory, Title: id,
		Description: description, Content: "content of " + id}
	require.NoError(t, client.CreateMemoryProposal(t.Context(), proposal))
	require.NoError(t, client.ReviewMemoryProposal(t.Context(), store.MemoryProposalReview{
		Namespace: "tenant", ID: id, Status: store.MemoryProposalStatusAccepted, Reviewer: "reviewer",
	}))
	return proposal
}

// TestApplyMemoryProposalAuthorizesTheMemoryAConcurrentApplyCreated races a
// second application between resolving "create a memory" and committing.
// The client must not return the winner's memory until the caller is
// authorized to read that exact memory.
func TestApplyMemoryProposalAuthorizesTheMemoryAConcurrentApplyCreated(t *testing.T) {
	client, backend := newProtocolClient(t)
	ctx := t.Context()
	proposal := acceptedProposal(t, client, "raced", "Tags: Alpha, beta, alpha\nTags: ignored")
	var winner *store.Memory
	backend.afterResolve = func() {
		var err error
		winner, err = client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: "tenant", ID: proposal.ID, AppliedBy: "winner"})
		require.NoError(t, err)
	}
	denied := errors.New("caller may not read this memory")
	var authorized []string
	memory, err := client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "tenant", ID: proposal.ID, AppliedBy: "late",
		AuthorizeExistingMemory: func(_ context.Context, namespace, id string) error {
			authorized = append(authorized, namespace+"/"+id)
			return denied
		},
	})
	require.ErrorIs(t, err, denied)
	require.Nil(t, memory, "an unauthorized caller receives no memory")
	require.NotNil(t, winner)
	require.Equal(t, []string{"tenant/" + winner.ID}, authorized, "the exact memory the commit would return is authorized")
	require.Equal(t, []string{"alpha", "beta"}, winner.Tags, "tags come from the first Tags line")

	memories, err := client.ListMemories(ctx, store.MemoryFilter{Namespace: "tenant", Source: store.MemorySourceProposal})
	require.NoError(t, err)
	require.Len(t, memories, 1, "the race creates one memory")
	applied, err := client.GetMemoryProposal(ctx, "tenant", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, "winner", applied.AppliedBy, "the denied caller changed nothing")

	memory, err = client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "tenant", ID: proposal.ID, AppliedBy: "late",
		AuthorizeExistingMemory: func(context.Context, string, string) error { return nil },
	})
	require.NoError(t, err)
	require.Equal(t, winner.ID, memory.ID)
}

func TestWithAuthorizedTaskDataAccessReportsAMovedFence(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := t.Context()
	handlerErr := errors.New("handler turned the refusal into its own error")
	err := client.WithAuthorizedTaskDataAccess(ctx, "tenant", "task", func(context.Context) ([]store.TaskJobIdentity, error) {
		return nil, nil
	}, func(fenced context.Context) error {
		require.NoError(t, client.DeleteResult(ctx, "tenant", "task"))
		if err := client.SaveResult(fenced, "tenant", "task", []byte("late")); err != nil {
			require.ErrorIs(t, err, store.ErrTaskDataCleanupChanged)
			return handlerErr
		}
		return nil
	})
	require.ErrorIs(t, err, store.ErrTaskDataCleanupChanged, "the caller sees the fence error and authorizes again")
	_, err = client.GetResult(ctx, "tenant", "task")
	require.ErrorIs(t, err, store.ErrNotFound)

	err = client.WithAuthorizedTaskDataAccess(ctx, "tenant", "task", func(context.Context) ([]store.TaskJobIdentity, error) {
		return nil, nil
	}, func(context.Context) error { return handlerErr })
	require.ErrorIs(t, err, handlerErr, "an access error with a current fence is returned unchanged")
}

func TestWithTaskDataAccessRegistersOnlyForTheAccess(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := t.Context()
	require.NoError(t, client.WithTaskDataAccess(ctx, "tenant", "task", func(fenced context.Context) error {
		_, registered, err := client.ResolveTaskDataFence(ctx, "tenant", "task")
		require.NoError(t, err)
		require.True(t, registered, "the access is registered while it runs")
		require.NoError(t, client.DeleteResult(ctx, "tenant", "other-task"))
		return client.SavePlan(fenced, "tenant", "task", &store.PlanState{Summary: "kept"})
	}), "cleanup of another Task does not refuse a registered access")
	_, registered, err := client.ResolveTaskDataFence(ctx, "tenant", "task")
	require.NoError(t, err)
	require.False(t, registered, "trusted controller access leaves no registration behind")
	plan, err := client.GetPlan(ctx, "tenant", "task")
	require.NoError(t, err)
	require.Equal(t, "kept", plan.Summary)
}

func TestScanIngestionRecordsReadsAndPreviewsOnlyAfterWrites(t *testing.T) {
	client, backend := newProtocolClient(t)
	ctx := t.Context()
	require.NoError(t, client.CreateScanRun(ctx, &store.ScanRun{ID: "run", Namespace: "tenant", RepositoryScan: "repo", Phase: "running"}))
	identity := store.ScanTaskIdentity{Namespace: "tenant", RepositoryScan: "repo", ScanRunID: "run", TaskName: "review", TaskUID: "uid", Stage: "review"}
	finding := &store.Finding{ID: "finding", Namespace: "tenant", RepositoryScan: "repo", ScanRunID: "run", Fingerprint: "finding", Title: "original", State: "open"}
	applied, err := client.ApplyScanTaskIngestion(ctx, &store.ScanTaskIngestion{ScanTaskIdentity: identity}, nil, func(tx store.SecurityStore, run *store.ScanRun) error {
		_, err := tx.GetFinding(ctx, "tenant", "finding")
		require.ErrorIs(t, err, store.ErrNotFound)
		require.Zero(t, backend.previews.Load(), "a read before any write goes to the provider directly")
		require.NoError(t, tx.UpsertObservedFinding(ctx, finding))
		stored, err := tx.GetFinding(ctx, "tenant", "finding")
		require.NoError(t, err)
		require.EqualValues(t, 1, backend.previews.Load(), "a read after a write of the same family is previewed")
		require.Equal(t, "original", stored.Title)
		stored.Title = "mutated by the callback"
		run.AcceptedFindings = 1
		return nil
	})
	require.NoError(t, err)
	require.True(t, applied, "mutating a returned record does not change what the commit checks")
	stored, err := client.GetFinding(ctx, "tenant", "finding")
	require.NoError(t, err)
	require.Equal(t, "original", stored.Title)

	second := identity
	second.TaskName, second.TaskUID = "unsupported", "unsupported-uid"
	commits := backend.commits.Load()
	applied, err = client.ApplyScanTaskIngestion(ctx, &store.ScanTaskIngestion{ScanTaskIdentity: second}, nil, func(tx store.SecurityStore, run *store.ScanRun) error {
		_ = tx.UpdateFindingState(ctx, "tenant", "finding", "dismissed")
		return nil
	})
	require.ErrorIs(t, err, store.ErrValidation, "a method that cannot join the commit fails the ingestion")
	require.False(t, applied)
	require.Equal(t, commits, backend.commits.Load(), "nothing is committed")
	stored, err = client.GetFinding(ctx, "tenant", "finding")
	require.NoError(t, err)
	require.Equal(t, "open", stored.State)
}

// A consolidation proposal changes memories nobody named in the apply
// request, so the client authorizes changing each of them before it commits.
func TestApplyMemoryProposalAuthorizesEveryConsolidationTarget(t *testing.T) {
	client, _ := newProtocolClient(t)
	ctx := t.Context()
	for _, id := range []string{"mem-a", "mem-b"} {
		require.NoError(t, client.CreateMemory(ctx, &store.Memory{ID: id, Namespace: "tenant", Content: "content of " + id}))
	}
	proposal := &store.MemoryProposal{ID: "merge", Namespace: "tenant", Type: store.MemoryProposalTypeMerge, Title: "Merge",
		Description: "Tags: ops", Content: "merged content",
		Targets: []store.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 1}}}
	require.NoError(t, client.CreateMemoryProposal(ctx, proposal))
	require.NoError(t, client.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: "tenant", ID: proposal.ID, Status: store.MemoryProposalStatusAccepted, Reviewer: "reviewer",
	}))

	denied := errors.New("caller may not change this memory")
	var checked []string
	memory, err := client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "tenant", ID: proposal.ID, AppliedBy: "applier",
		AuthorizeMemoryChange: func(_ context.Context, namespace, id string) error {
			checked = append(checked, namespace+"/"+id)
			if id == "mem-b" {
				return denied
			}
			return nil
		},
	})
	require.ErrorIs(t, err, denied)
	require.Nil(t, memory)
	require.Equal(t, []string{"tenant/mem-a", "tenant/mem-b"}, checked)
	for _, id := range []string{"mem-a", "mem-b"} {
		unchanged, err := client.GetMemory(ctx, "tenant", id)
		require.NoError(t, err)
		require.Equal(t, uint64(1), unchanged.Generation, "a denied application changes nothing")
		require.False(t, unchanged.Deleted)
	}

	memory, err = client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "tenant", ID: proposal.ID, AppliedBy: "applier",
		AuthorizeMemoryChange: func(context.Context, string, string) error { return nil },
	})
	require.NoError(t, err)
	require.Equal(t, "merged content", memory.Content)
	require.Equal(t, []string{"ops"}, memory.Tags, "a merged memory takes its tags from the proposal, as a memory proposal does")
	retired, err := client.GetMemory(ctx, "tenant", "mem-a")
	require.NoError(t, err)
	require.Equal(t, memory.ID, retired.SupersededBy)

	checked = nil
	again, err := client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{
		Namespace: "tenant", ID: proposal.ID, AppliedBy: "applier",
		AuthorizeMemoryChange: func(_ context.Context, namespace, id string) error {
			checked = append(checked, id)
			return nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, memory.ID, again.ID)
	require.Empty(t, checked, "replaying an applied proposal changes no memory, so it authorizes none")
}

// A stale consolidation proposal fails once with a conflict: resolving again
// cannot make its targets current.
func TestApplyMemoryProposalRefusesAStaleConsolidation(t *testing.T) {
	client, _ := newProtocolClient(t)
	ctx := t.Context()
	require.NoError(t, client.CreateMemory(ctx, &store.Memory{ID: "mem-a", Namespace: "tenant", Content: "first"}))
	proposal := &store.MemoryProposal{ID: "retire", Namespace: "tenant", Type: store.MemoryProposalTypeRetire, Title: "Retire",
		Targets: []store.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}}}
	require.NoError(t, client.CreateMemoryProposal(ctx, proposal))
	require.NoError(t, client.ReviewMemoryProposal(ctx, store.MemoryProposalReview{
		Namespace: "tenant", ID: proposal.ID, Status: store.MemoryProposalStatusAccepted, Reviewer: "reviewer",
	}))
	require.NoError(t, client.UpdateMemory(ctx, &store.Memory{ID: "mem-a", Namespace: "tenant", Content: "edited"}))

	memory, err := client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: "tenant", ID: proposal.ID, AppliedBy: "applier"})
	require.ErrorIs(t, err, store.ErrConflict)
	require.Nil(t, memory)
	current, err := client.GetMemory(ctx, "tenant", "mem-a")
	require.NoError(t, err)
	require.False(t, current.Disabled)
	require.Equal(t, "edited", current.Content)
}
